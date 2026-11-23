// Package data 生成仿真键与查询负载。
//
// 四种分布：
//   - timestamps：交易时间戳（毫秒）。白天密、夜里稀、有突发高峰、大量重复/邻近值；
//   - uniform：用户编号在大整数域上近似均匀（带少量重复）；
//   - zipf：少数用户编号高频（Zipf 分布），重复值极多；
//   - geo：地理编码，用经纬度混合高斯聚簇后做 Morton（Z-order）交织成一维键。
package data

import (
	"math"
	"math/rand"
	"sort"
)

// Profile 标识一种键分布。
type Profile string

const (
	Timestamps Profile = "timestamps"
	Uniform    Profile = "uniform"
	Zipf       Profile = "zipf"
	Geo        Profile = "geo"
)

// Profiles 返回全部可用分布。
func Profiles() []Profile {
	return []Profile{Timestamps, Uniform, Zipf, Geo}
}

const (
	dayMillis    = 24 * 3600 * 1000
	spanDays     = 7
	tsDomainSize = int64(spanDays) * dayMillis // 一周的毫秒域
)

// rng：自带确定性 PRNG，保证每次跑同一 seed 得到同一批数据。
type rng struct{ r *rand.Rand }

func newRNG(seed int64) *rng { return &rng{r: rand.New(rand.NewSource(seed))} }

// hourWeight 模拟一天内的交易密度：夜间低谷、午间与晚间两个高峰。
func hourWeight(hour int) float64 {
	switch {
	case hour < 6:
		return 0.15
	case hour < 9:
		return 0.6 + 0.25*float64(hour-6)/3
	case hour < 12:
		return 1.1
	case hour < 14:
		return 1.5 // 午间小高峰
	case hour < 18:
		return 1.0
	case hour < 21:
		return 1.7 // 晚间主高峰
	default:
		return 0.5
	}
}

// genTimestamp 按小时权重抽一个“毫秒偏移”，再叠加分钟级抖动。
func (g *rng) genTimestamp() int64 {
	// 先在 24*7 个小时桶上按权重选桶。
	totalHours := spanDays * 24
	weights := make([]float64, totalHours)
	var sum float64
	for h := 0; h < totalHours; h++ {
		w := hourWeight(h % 24)
		weights[h] = w
		sum += w
	}
	pick := g.r.Float64() * sum
	hour := 0
	acc := 0.0
	for ; hour < totalHours; hour++ {
		acc += weights[hour]
		if acc >= pick {
			break
		}
	}
	if hour >= totalHours {
		hour = totalHours - 1
	}
	within := g.r.Int63n(3600 * 1000)
	return int64(hour)*3600*1000 + within
}

// Generate 生成 n 个指定分布的键并排序。dupFraction 控制重复键比例。
func Generate(p Profile, n int, seed int64) []int64 {
	g := newRNG(seed)
	keys := make([]int64, n)
	switch p {
	case Timestamps:
		// 8% 完全重复；另有大量邻近值（同一秒内）。
		const dupFrac = 0.08
		nDup := int(float64(n) * dupFrac)
		for i := 0; i < n; i++ {
			keys[i] = g.genTimestamp()
		}
		// 突发高峰：在三个时间点注入尖峰（大量键挤进同一秒）。
		bursts := []int64{
			int64(2*24+10) * 3600 * 1000,
			int64(4*24+19) * 3600 * 1000,
			int64(6*24+13) * 3600 * 1000,
		}
		nBurst := n / 20
		for i := 0; i < nBurst; i++ {
			b := bursts[i%len(bursts)]
			keys[n-1-i] = b + g.r.Int63n(1000)
		}
		for i := 0; i < nDup; i++ {
			keys[i] = keys[g.r.Intn(n)]
		}
	case Uniform:
		domain := int64(1_000_000_000)
		nDup := n / 50
		for i := 0; i < n; i++ {
			keys[i] = g.r.Int63n(domain)
		}
		for i := 0; i < nDup; i++ {
			keys[i] = keys[g.r.Intn(n)]
		}
	case Zipf:
		// 用户编号域 1e9，频次按 Zipf（s≈1.02）衰减，少量热门用户贡献大量重复。
		zipfDomain := int64(1_000_000_000)
		s := 1.02
		hm := harmonic(zipfDomain, s)
		for i := 0; i < n; i++ {
			keys[i] = zipfSample(g, zipfDomain, s, hm)
		}
	case Geo:
		// 三个城市中心的二维高斯聚簇，经纬度量化到 ~1e4 网格后做 Morton 交织。
		centers := [][2]float64{{39.9, 116.4}, {31.2, 121.5}, {22.5, 114.1}}
		nDup := n / 12
		for i := 0; i < n; i++ {
			c := centers[g.r.Intn(len(centers))]
			lat := c[0] + g.r.NormFloat64()*0.8
			lon := c[1] + g.r.NormFloat64()*1.0
			keys[i] = morton(quantize(lat, -90, 90), quantize(lon, -180, 180))
		}
		for i := 0; i < nDup; i++ {
			keys[i] = keys[g.r.Intn(n)]
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func quantize(v, lo, hi float64) uint32 {
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return uint32((v - lo) / (hi - lo) * (1<<31 - 1))
}

// partBy1 把 32 位整数的比特分散到 64 位的偶数位（Morton 交织用）。
func partBy1(x uint32) uint64 {
	v := uint64(x) & 0x1fffffff
	v = (v | (v << 16)) & 0x030000ff030000ff
	v = (v | (v << 8)) & 0x0300f00f0300f00f
	v = (v | (v << 4)) & 0x030c30c3030c30c3
	v = (v | (v << 2)) & 0x0924924909249249
	return v
}

// morton 把两个 31 位坐标交织成一个 62 位非负 int64。
func morton(a, b uint32) int64 {
	return int64(partBy1(a) | (partBy1(b) << 1))
}

func harmonic(n int64, s float64) float64 {
	// 大 n 时用积分近似，避免 O(n) 求和。
	if n > 1_000_000 {
		return (math.Pow(float64(n), 1-s) - 1) / (1 - s)
	}
	var h float64
	for k := int64(1); k <= n; k++ {
		h += 1 / math.Pow(float64(k), s)
	}
	return h
}

// zipfSample 用逆 CDF 近似抽样：P(K=k) ∝ 1/k^s。
func zipfSample(g *rng, n int64, s, hm float64) int64 {
	u := g.r.Float64() * hm
	if math.Abs(1-s) < 1e-6 {
		// s≈1：CDF ≈ ln(k)，逆函数为 exp。
		k := int64(math.Exp(u))
		if k < 1 {
			k = 1
		}
		if k > n {
			k = n
		}
		return k
	}
	// CDF ≈ (k^(1-s) - 1)/(1-s)
	k := math.Pow((1-s)*u+1, 1/(1-s))
	v := int64(math.Ceil(k))
	if v < 1 {
		v = 1
	}
	if v > n {
		v = n
	}
	return v
}

// Workload 是一组查询负载。
type Workload struct {
	PointKeys []int64 // 一半命中（取自数据），一半随机（多为不存在的键）
	RangeLos  []int64
	RangeHis  []int64
}

// GenWorkload 生成点查与范围查负载。
func GenWorkload(p Profile, sortedKeys []int64, q int, seed int64) Workload {
	g := newRNG(seed + 99)
	w := Workload{PointKeys: make([]int64, q)}
	n := len(sortedKeys)
	domain := keyDomain(p)
	for i := 0; i < q; i++ {
		if i%2 == 0 && n > 0 {
			w.PointKeys[i] = sortedKeys[g.r.Intn(n)] // 命中
		} else {
			w.PointKeys[i] = g.r.Int63n(domain) // 大概率不存在
		}
	}
	w.RangeLos = make([]int64, q/4)
	w.RangeHis = make([]int64, q/4)
	for i := range w.RangeLos {
		lo := g.r.Int63n(domain)
		// 范围宽度：毫秒域取 1 分钟~6 小时；其他域取域宽的 0.01%~1%。
		var width int64
		if p == Timestamps {
			width = 60_000 + g.r.Int63n(6*3600*1000)
		} else {
			width = domain/1_000_000 + g.r.Int63n(domain/100)
		}
		w.RangeLos[i] = lo
		w.RangeHis[i] = lo + width
	}
	return w
}

func keyDomain(p Profile) int64 {
	switch p {
	case Timestamps:
		return tsDomainSize
	case Geo:
		return 1 << 62
	default:
		return 1_000_000_000
	}
}
