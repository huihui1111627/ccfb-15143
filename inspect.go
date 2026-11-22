package vdisk

// 本文件构造 Web 页面与命令行共用的镜像内部结构视图。

// L1View 是一级映射表的占用情况。
type L1View struct {
	Clusters       int64 `json:"clusters"`
	AllocatedL2    int64 `json:"allocatedL2"`    // 已指向 L2 的项数
	TotalEntries   int64 `json:"totalEntries"`   // 虚拟簇总数
	EntriesPerTbl  int64 `json:"entriesPerTable"`
}

// RefL1View 是引用计数一级表的占用情况。
type RefL1View struct {
	Clusters        int64 `json:"clusters"`
	AllocatedRefL2  int64 `json:"allocatedRefL2"`
	TotalEntries    int64 `json:"maxClusters"`
	EntriesPerTbl   int64 `json:"entriesPerTable"`
}

// L2Detail 是单个二级表的明细。
type L2Detail struct {
	VirtualCluster int64 `json:"virtualCluster"` // 该 L2 负责的首个虚拟簇号
	PhysCluster    int64 `json:"physCluster"`    // L2 自身的物理簇号
	ValidEntries   int64 `json:"validEntries"`   // 指向数据簇的项数
	ZeroEntries    int64 `json:"zeroEntries"`    // 全零标记项数
	HoleEntries    int64 `json:"holeEntries"`    // 未分配项数
}

// RefL2Detail 是单个引用计数二级表的明细。
type RefL2Detail struct {
	Index       int64 `json:"index"`
	PhysCluster int64 `json:"physCluster"`
	NonZeroRefs int64 `json:"nonZeroRefs"`
}

// ClusterRef 是单个物理簇的引用计数视图。
type ClusterRef struct {
	Cluster int64 `json:"cluster"`
	Role    string `json:"role"`
	Refs    int64  `json:"refs"`
	Free    bool   `json:"free"`
}

// InspectReport 是镜像内部结构的完整快照。
type InspectReport struct {
	Path            string      `json:"path"`
	BasePath        string      `json:"basePath"`
	ClusterSize     int         `json:"clusterSize"`
	VirtualSize     int64       `json:"virtualSize"`
	FileBytes       int64       `json:"fileBytes"`
	HighWater       int64       `json:"highWater"`
	MaxClusters     int64       `json:"maxClusters"`
	Dirty           bool        `json:"dirty"`
	ReadOnly        bool        `json:"readOnly"`
	Extensions      []ExtItem   `json:"extensions"`
	L1              L1View      `json:"l1"`
	RefL1           RefL1View   `json:"refL1"`
	L2Tables        []L2Detail  `json:"l2Tables"`
	RefL2Tables     []RefL2Detail `json:"refL2Tables"`
	ClusterRefs     []ClusterRef `json:"clusterRefs"`
	DataClusters    int64       `json:"dataClusters"`
	ZeroMarkedVC    int64       `json:"zeroMarkedVirtualClusters"`
	LeakedClusters  int64       `json:"leakedClusters"`
}

// Inspect 生成当前镜像的结构快照（只读，不改镜像）。
func (img *Image) Inspect(path string) (*InspectReport, error) {
	img.mu.RLock()
	defer img.mu.RUnlock()

	r := &InspectReport{
		Path:        path,
		BasePath:    img.basePath,
		ClusterSize: img.clusterSize,
		VirtualSize: img.VirtualSize(),
		FileBytes:   img.blk.Size(),
		HighWater:   img.highWater,
		MaxClusters: img.maxClusters,
		Dirty:       img.dirty,
		ReadOnly:    img.readOnly,
		Extensions:  img.extensionsLocked(),
	}
	r.L1 = L1View{
		Clusters:      img.l1Clusters(),
		TotalEntries:  img.virtualClusters,
		EntriesPerTbl: img.entriesPer,
	}
	r.RefL1 = RefL1View{
		Clusters:      img.refL1Clusters(),
		TotalEntries:  img.maxClusters,
		EntriesPerTbl: img.entriesPer,
	}

	role := map[int64]string{}
	for c := img.l1Start; c < img.l1Start+img.l1Clusters(); c++ {
		role[c] = "L1"
	}
	for c := img.refL1Start; c < img.refL1Start+img.refL1Clusters(); c++ {
		role[c] = "refL1"
	}
	for c := img.refL2Start; c < img.refL2Start+img.refL1Clusters(); c++ {
		role[c] = "refL2"
	}

	// L2 明细（按组）。
	l1Groups := (img.virtualClusters + img.entriesPer - 1) / img.entriesPer
	for g := int64(0); g < l1Groups; g++ {
		l2c := img.l1[g]
		if l2c == 0 {
			continue
		}
		r.L1.AllocatedL2++
		role[l2c] = "L2"
		groupBase := g * img.entriesPer
		d := L2Detail{VirtualCluster: groupBase, PhysCluster: l2c}
		buf, err := img.readCluster(l2c)
		if err != nil {
			return nil, err
		}
		for j := int64(0); j < img.entriesPer; j++ {
			if groupBase+j >= img.virtualClusters {
				break
			}
			raw := readEntry(buf, j)
			target, isZero, err := img.validateMapEntry(raw, "L2")
			if err != nil {
				return nil, err
			}
			switch {
			case isZero:
				d.ZeroEntries++
				r.ZeroMarkedVC++
			case target != 0:
				d.ValidEntries++
				r.DataClusters++
				role[target] = "data"
			default:
				d.HoleEntries++
			}
		}
		r.L2Tables = append(r.L2Tables, d)
	}

	// refL2 明细（按组，每组一个表簇）。
	for g := int64(0); g < img.refL1Clusters(); g++ {
		refL2 := img.refL2Start + g
		r.RefL1.AllocatedRefL2++
		role[refL2] = "refL2"
		d := RefL2Detail{Index: g, PhysCluster: refL2}
		buf, err := img.readCluster(refL2)
		if err != nil {
			return nil, err
		}
		for j := int64(0); j < img.entriesPer; j++ {
			pc := g*img.entriesPer + j
			if pc >= img.maxClusters {
				break
			}
			v, err := validateRefEntry(readEntry(buf, j), "refL2")
			if err != nil {
				return nil, err
			}
			if v != 0 {
				d.NonZeroRefs++
			}
		}
		r.RefL2Tables = append(r.RefL2Tables, d)
	}

	// 每个物理簇的引用计数。
	for c := int64(firstDataCluster); c < img.highWater; c++ {
		refs := img.refCount[c]
		_, free := img.freeSet[c]
		cr := ClusterRef{
			Cluster: c,
			Role:    role[c],
			Refs:    refs,
			Free:    free,
		}
		if cr.Role == "" {
			cr.Role = "unknown"
		}
		if refs == 0 && !img.isRootCluster(c) {
			r.LeakedClusters++
		}
		r.ClusterRefs = append(r.ClusterRefs, cr)
	}
	return r, nil
}

// Stats 返回逻辑/物理 IO 统计与读写放大。
func (img *Image) Stats() Stats {
	img.mu.RLock()
	defer img.mu.RUnlock()
	s := Stats{
		LogicalReadBytes:  img.logicalR,
		LogicalWriteBytes: img.logicalW,
	}
	if img.sb != nil {
		s.PhysicalReadBytes = img.sb.readBytes.Load()
		s.PhysicalWriteBytes = img.sb.writeBytes.Load()
		s.SyncCount = img.sb.syncs.Load()
	}
	return s
}
