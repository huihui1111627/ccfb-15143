// Package web 提供可视化 HTTP 服务：键分布曲线、PLA 分段直线与每段误差、
// 多层结构，以及一次点查的逐层预测过程。
package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"

	"learnedindex/internal/data"
	"learnedindex/internal/learned"
)

// Server 持有一个小样本数据集与对应索引，供页面可视化（样本规模受控，
// 避免把千万级数据塞进浏览器；大数据的统计结论由 benchmark 给出）。
type Server struct {
	mux *http.ServeMux
}

// New 构建可视化服务。sampleN 为样本键数量。
func New(sampleN int) *Server {
	s := &Server{mux: http.NewServeMux()}

	type profileCache struct {
		keys []int64
		idx  map[int]*learned.StaticIndex
	}
	caches := map[data.Profile]*profileCache{}
	getCache := func(p data.Profile) *profileCache {
		if c, ok := caches[p]; ok {
			return c
		}
		keys := data.Generate(p, sampleN, 20261123)
		c := &profileCache{keys: keys, idx: map[int]*learned.StaticIndex{}}
		caches[p] = c
		return c
	}

	s.mux.HandleFunc("/api/profiles", func(w http.ResponseWriter, r *http.Request) {
		type pf struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		out := []pf{
			{string(data.Timestamps), "交易时间戳（昼密夜稀+突发高峰+重复）"},
			{string(data.Uniform), "用户编号（近似均匀）"},
			{string(data.Zipf), "热门用户编号（Zipf，重复极多）"},
			{string(data.Geo), "地理编码（Morton 交织，聚簇）"},
		}
		writeJSON(w, out)
	})

	s.mux.HandleFunc("/api/distribution", func(w http.ResponseWriter, r *http.Request) {
		p := data.Profile(r.URL.Query().Get("profile"))
		buckets := atoi(r.URL.Query().Get("buckets"), 120)
		c := getCache(p)
		writeJSON(w, histogram(c.keys, buckets))
	})

	s.mux.HandleFunc("/api/segments", func(w http.ResponseWriter, r *http.Request) {
		p := data.Profile(r.URL.Query().Get("profile"))
		eps := atoi(r.URL.Query().Get("eps"), 32)
		c := getCache(p)
		idx, ok := c.idx[eps]
		if !ok {
			idx = learned.BuildStatic(c.keys, eps)
			c.idx[eps] = idx
		}
		writeJSON(w, segmentView(c.keys, idx))
	})

	s.mux.HandleFunc("/api/trace", func(w http.ResponseWriter, r *http.Request) {
		p := data.Profile(r.URL.Query().Get("profile"))
		eps := atoi(r.URL.Query().Get("eps"), 32)
		key, err := strconv.ParseInt(r.URL.Query().Get("key"), 10, 64)
		if err != nil {
			http.Error(w, "bad key", http.StatusBadRequest)
			return
		}
		c := getCache(p)
		idx, ok := c.idx[eps]
		if !ok {
			idx = learned.BuildStatic(c.keys, eps)
			c.idx[eps] = idx
		}
		steps, res := idx.Trace(key)
		writeJSON(w, map[string]any{"steps": steps, "result": res})
	})

	staticFS, err := fs.Sub(siteFS, "site")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))
	return s
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// histogram 把有序键压成 buckets 个等宽桶，返回每桶键数（密度曲线）。
func histogram(keys []int64, buckets int) map[string]any {
	n := len(keys)
	lo, hi := keys[0], keys[n-1]
	width := float64(hi-lo) / float64(buckets)
	counts := make([]int, buckets)
	for _, k := range keys {
		b := int(float64(k-lo) / width)
		if b >= buckets {
			b = buckets - 1
		}
		counts[b]++
	}
	type bucket struct {
		X     float64 `json:"x"`
		Count int     `json:"count"`
	}
	out := make([]bucket, buckets)
	for i, c := range counts {
		out[i] = bucket{X: float64(lo) + width*(float64(i)+0.5), Count: c}
	}
	uniq := 1
	for i := 1; i < n; i++ {
		if keys[i] != keys[i-1] {
			uniq++
		}
	}
	return map[string]any{
		"buckets": out, "min": lo, "max": hi, "n": n,
		"unique": uniq, "dupRatio": 1 - float64(uniq)/float64(n),
	}
}

// segmentView 汇总分段直线、每段误差与多层结构，供前端绘制。
func segmentView(keys []int64, idx *learned.StaticIndex) map[string]any {
	segs := idx.LeafSegments()
	type segJSON struct {
		First     int64   `json:"first"`
		Last      int64   `json:"last"`
		StartPos  int64   `json:"startPos"`
		EndPos    int64   `json:"endPos"`
		Slope     float64 `json:"slope"`
		Intercept float64 `json:"intercept"`
		MaxErr    float64 `json:"maxErr"`
		Points    int     `json:"points"`
	}
	out := make([]segJSON, 0, len(segs))
	var totalErr float64
	for _, sg := range segs {
		maxErr := 0.0
		for j := sg.StartPos; j <= sg.EndPos; j++ {
			e := sg.Predict(keys[j]) - float64(j)
			if e < 0 {
				e = -e
			}
			if e > maxErr {
				maxErr = e
			}
			totalErr += e
		}
		out = append(out, segJSON{
			First: sg.First, Last: sg.Last,
			StartPos: sg.StartPos, EndPos: sg.EndPos,
			Slope: sg.Slope, Intercept: sg.Intercept,
			MaxErr: maxErr, Points: int(sg.EndPos - sg.StartPos + 1),
		})
	}
	// 抽样部分真实 CDF 点（键->位置），与拟合直线叠加显示。
	const samplePoints = 600
	type pt struct {
		X int64 `json:"x"`
		Y int   `json:"y"`
	}
	pts := make([]pt, 0, samplePoints+1)
	step := len(keys) / samplePoints
	if step < 1 {
		step = 1
	}
	for i := 0; i < len(keys); i += step {
		pts = append(pts, pt{X: keys[i], Y: i})
	}
	pts = append(pts, pt{X: keys[len(keys)-1], Y: len(keys) - 1})

	return map[string]any{
		"segments":   out,
		"cdf":        pts,
		"layers":     idx.SegmentCounts(),
		"eps":        idx.Eps(),
		"indexBytes": idx.IndexSizeBytes(),
		"avgErr":     totalErr / float64(len(keys)),
	}
}
