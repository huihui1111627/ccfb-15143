package vdisk

// 测试用的确定性随机源，避免直接依赖 math/rand 造成包内耦合。
type testRng struct{ state uint64 }

func newRng(seed uint64) *testRng {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &testRng{state: seed}
}

func (r *testRng) next() uint64 {
	// xorshift64*
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}

func (r *testRng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *testRng) fill(b []byte) {
	for i := range b {
		b[i] = byte(r.next())
	}
}
