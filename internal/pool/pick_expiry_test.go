package pool

// 到期偏好策略（②）的单元测试：三档系数、金额占比、扣积分闸门、相对上界、
// 「濒临作废」硬层、排序键、粘性虚拟实例、总开关零回归、以及分布倾斜。

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// expiryTestPool 构造一个"该模型在号上确实收费"的最小池子（闸门需要 tier 2 证据）。
func expiryTestPool(t *testing.T, uids ...string) *Pool {
	t.Helper()
	p := New("")
	now := time.Now()
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
	}
	p.mu.Lock()
	for _, uid := range uids {
		if e := p.byUID[uid]; e != nil {
			e.modelCost = map[string]modelCostEntry{"m": {CostPer1k: 1, LastSeen: now, Samples: 2}}
		}
	}
	p.mu.Unlock()
	return p
}

// TestExpiryTierFactor 三档系数 × 金额占比因子。
func TestExpiryTierFactor(t *testing.T) {
	p := expiryTestPool(t, "fast", "mid", "slow", "none")
	now := time.Now()
	p.SetCreditsDetailed("fast", CreditsSnapshot{Remain: 1000, Total: 1000, Expiring: 400, Expiring24h: 100, Expiring72h: 300, EarliestAt: now.Add(2 * time.Hour), EarliestRemaining: 100})
	p.SetCreditsDetailed("mid", CreditsSnapshot{Remain: 1000, Total: 1000, Expiring: 400, Expiring24h: 0, Expiring72h: 300, EarliestAt: now.Add(48 * time.Hour), EarliestRemaining: 300})
	p.SetCreditsDetailed("slow", CreditsSnapshot{Remain: 1000, Total: 1000, Expiring: 400, EarliestAt: now.Add(120 * time.Hour), EarliestRemaining: 400})
	p.SetCreditsDetailed("none", CreditsSnapshot{Remain: 1000, Total: 1000})

	p.mu.Lock()
	defer p.mu.Unlock()
	cases := []struct {
		uid  string
		tier int
		// 占比 = min(1, Expiring/credits × 2) = min(1, 0.8) = 0.8
		boost float64
	}{
		{"fast", expiryTierFast, 6 * 0.8},
		{"mid", expiryTierMid, 4 * 0.8},
		{"slow", expiryTierSlow, 3 * 0.8},
		{"none", expiryTierNone, 1},
	}
	for _, c := range cases {
		tier, boost := p.expiryBoostLocked(p.byUID[c.uid], "m", now)
		if tier != c.tier {
			t.Fatalf("%s tier=%d want %d", c.uid, tier, c.tier)
		}
		if diff := boost - c.boost; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("%s boost=%v want %v", c.uid, boost, c.boost)
		}
	}
}

// TestExpiryGateSkipsNonCharging 闸门：免费模型 / 无观测且无牌价时不加权。
func TestExpiryGateSkipsNonCharging(t *testing.T) {
	now := time.Now()
	snap := CreditsSnapshot{Remain: 100, Total: 100, Expiring: 100, Expiring24h: 100, Expiring72h: 100, EarliestAt: now.Add(time.Hour), EarliestRemaining: 100}

	// 1) 该号在该模型上实测免费（tier 0）→ 不加权。
	p := New("")
	p.Add(&auth.Auth{UID: "free"})
	p.SetCreditsDetailed("free", snap)
	p.mu.Lock()
	p.byUID["free"].modelCost = map[string]modelCostEntry{"m": {CostPer1k: 0, LastSeen: now, Samples: 2}}
	_, boost := p.expiryBoostLocked(p.byUID["free"], "m", now)
	p.mu.Unlock()
	if boost != 1 {
		t.Fatalf("free model boost=%v want 1", boost)
	}

	// 2) 无观测、无牌价回调 → 未知收费 → 不加权。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "unknown"})
	p2.SetCreditsDetailed("unknown", snap)
	p2.mu.Lock()
	_, boost = p2.expiryBoostLocked(p2.byUID["unknown"], "m", now)
	p2.mu.Unlock()
	if boost != 1 {
		t.Fatalf("unknown rate boost=%v want 1", boost)
	}

	// 3) 无观测但上游牌价 > 0 → 视为收费 → 加权。
	p3 := New("")
	p3.Add(&auth.Auth{UID: "priced"})
	p3.SetCreditsDetailed("priced", snap)
	p3.SetModelRateOf(func(realm, model string) string { return "1.62" })
	p3.mu.Lock()
	_, boost = p3.expiryBoostLocked(p3.byUID["priced"], "m", now)
	p3.mu.Unlock()
	if boost <= 1 {
		t.Fatalf("priced model boost=%v want >1", boost)
	}
}

// TestExpiryHardLayer 硬层阈值：按实测号池校准的双条件（绝对金额 + 余额占比）。
func TestExpiryHardLayer(t *testing.T) {
	now := time.Now()
	p := expiryTestPool(t, "drip", "chunk", "small")
	// 实测滴水号：24h 内到期 210，余额 11732 → 1.8%，不应触发。
	p.SetCreditsDetailed("drip", CreditsSnapshot{Remain: 11732, Total: 20000, Expiring: 1576, Expiring24h: 210, Expiring72h: 529, EarliestAt: now.Add(11 * time.Hour), EarliestRemaining: 10})
	// 大块号：24h 内到期 0（1500 在 6 天后）→ 不应触发（交给软权重）。
	p.SetCreditsDetailed("chunk", CreditsSnapshot{Remain: 9092, Total: 20000, Expiring: 3750, EarliestAt: now.Add(148 * time.Hour), EarliestRemaining: 1500})
	// 小号：余额 800，24h 内到期 300 → 300 ≥ 200 且 37.5% ≥ 20% → 触发。
	p.SetCreditsDetailed("small", CreditsSnapshot{Remain: 800, Total: 800, Expiring: 300, Expiring24h: 300, Expiring72h: 300, EarliestAt: now.Add(20 * time.Hour), EarliestRemaining: 300})

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hardExpiringLocked(p.byUID["drip"], "m", now) {
		t.Fatal("drip (210/11732=1.8%) must NOT trigger hard layer")
	}
	if p.hardExpiringLocked(p.byUID["chunk"], "m", now) {
		t.Fatal("chunk (0 in 24h) must NOT trigger hard layer")
	}
	if !p.hardExpiringLocked(p.byUID["small"], "m", now) {
		t.Fatal("small (300/800=37.5%) must trigger hard layer")
	}
}

// TestExpiryHardLayerFallsBackWhenTried 硬层组被 tried 排空时回落普通路径，不返回 nil。
func TestExpiryHardLayerFallsBackWhenTried(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := expiryTestPool(t, "hard", "plain")
	p.SetCreditsDetailed("hard", CreditsSnapshot{Remain: 800, Total: 800, Expiring: 500, Expiring24h: 500, Expiring72h: 500, EarliestAt: now.Add(2 * time.Hour), EarliestRemaining: 500})
	p.SetCreditsDetailed("plain", CreditsSnapshot{Remain: 100, Total: 100})

	got := p.PickExcluding(map[string]bool{"hard": true})
	if got == nil || got.UID != "plain" {
		t.Fatalf("pick=%v want plain (hard layer drained by tried)", got)
	}
}

// TestExpiryBoostCap 相对上界：被加权的号不超过同层基准权重中位数 × BoostCap。
func TestExpiryBoostCap(t *testing.T) {
	now := time.Now()
	p := expiryTestPool(t, "a", "b", "c")
	// 三号余额相同 → 基准权重相同（闲置已钳 0）；a 快档满额（×6）。
	p.mu.Lock()
	p.idleWeightPerHour = 0
	p.idleWeightMax = 0
	p.mu.Unlock()
	p.SetCreditsDetailed("a", CreditsSnapshot{Remain: 100, Total: 100, Expiring: 100, Expiring24h: 100, Expiring72h: 100, EarliestAt: now.Add(time.Hour), EarliestRemaining: 100})
	p.SetCreditsDetailed("b", CreditsSnapshot{Remain: 100, Total: 100})
	p.SetCreditsDetailed("c", CreditsSnapshot{Remain: 100, Total: 100})

	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := pickCtx{now: now, model: "m"}
	out, _ := p.rankLocked(ctx, []*entry{p.byUID["a"], p.byUID["b"], p.byUID["c"]})
	var got *scored
	for i := range out {
		if out[i].e.a.UID == "a" {
			got = &out[i]
		}
	}
	if got == nil {
		t.Fatal("a missing from ranked output")
	}
	// 基准 11（1 + 100/100×10），上界 4×11=44；未钳制应为 66。
	if got.w > 44+1e-6 {
		t.Fatalf("boosted weight=%v want ≤44 (4×median)", got.w)
	}
	if got.w < 44-1e-6 {
		t.Fatalf("boosted weight=%v want exactly 44 (cap applied)", got.w)
	}
}

// TestExpirySortPutsFastFirst 排序键：紧迫号即使余额低也进 top5 首位。
func TestExpirySortPutsFastFirst(t *testing.T) {
	now := time.Now()
	p := expiryTestPool(t, "rich", "urgent")
	p.mu.Lock()
	p.idleWeightPerHour = 0
	p.idleWeightMax = 0
	p.mu.Unlock()
	p.SetCreditsDetailed("rich", CreditsSnapshot{Remain: 100000, Total: 100000})
	p.SetCreditsDetailed("urgent", CreditsSnapshot{Remain: 100, Total: 100, Expiring: 100, Expiring24h: 100, Expiring72h: 100, EarliestAt: now.Add(3 * time.Hour), EarliestRemaining: 100})

	p.mu.Lock()
	defer p.mu.Unlock()
	out, _ := p.rankLocked(pickCtx{now: now, model: "m"}, []*entry{p.byUID["rich"], p.byUID["urgent"]})
	if len(out) != 2 {
		t.Fatalf("ranked len=%d want 2", len(out))
	}
	if out[0].e.a.UID != "urgent" {
		t.Fatalf("top=%s want urgent", out[0].e.a.UID)
	}
}

// TestExpirySortIgnoresTrivialBoost 微不足道的到期量只影响权重、不影响位次。
func TestExpirySortIgnoresTrivialBoost(t *testing.T) {
	now := time.Now()
	p := expiryTestPool(t, "rich", "trivial")
	p.mu.Lock()
	p.idleWeightPerHour = 0
	p.idleWeightMax = 0
	p.mu.Unlock()
	p.SetCreditsDetailed("rich", CreditsSnapshot{Remain: 100000, Total: 100000})
	// 余额 10000，24h 内到期 1 → 占比 0.0002 → boost≈0.0012 < 1.5 → 不进排序优先。
	p.SetCreditsDetailed("trivial", CreditsSnapshot{Remain: 10000, Total: 10000, Expiring: 1, Expiring24h: 1, Expiring72h: 1, EarliestAt: now.Add(3 * time.Hour), EarliestRemaining: 1})

	p.mu.Lock()
	defer p.mu.Unlock()
	out, _ := p.rankLocked(pickCtx{now: now, model: "m"}, []*entry{p.byUID["rich"], p.byUID["trivial"]})
	if out[0].e.a.UID != "rich" {
		t.Fatalf("top=%s want rich (trivial boost must not reorder)", out[0].e.a.UID)
	}
}

// TestExpirySlots 粘性分配的虚拟实例按档位折算（Q20a：24h→3、72h/窗口→2、无→1）。
func TestExpirySlots(t *testing.T) {
	now := time.Now()
	p := expiryTestPool(t, "fast", "mid", "slow", "none")
	p.SetCreditsDetailed("fast", CreditsSnapshot{Remain: 100, Total: 100, Expiring: 100, Expiring24h: 100, Expiring72h: 100, EarliestAt: now.Add(time.Hour), EarliestRemaining: 100})
	p.SetCreditsDetailed("mid", CreditsSnapshot{Remain: 100, Total: 100, Expiring: 100, Expiring24h: 0, Expiring72h: 100, EarliestAt: now.Add(40 * time.Hour), EarliestRemaining: 100})
	p.SetCreditsDetailed("slow", CreditsSnapshot{Remain: 100, Total: 100, Expiring: 100, EarliestAt: now.Add(120 * time.Hour), EarliestRemaining: 100})
	p.SetCreditsDetailed("none", CreditsSnapshot{Remain: 100, Total: 100})

	uids := p.WeightedAvailableUIDsForModelRealm("m", "")
	count := map[string]int{}
	for _, uid := range uids {
		count[uid]++
	}
	want := map[string]int{"fast": 3, "mid": 2, "slow": 2, "none": 1}
	for uid, n := range want {
		if count[uid] != n {
			t.Fatalf("%s slots=%d want %d (all=%v)", uid, count[uid], n, count)
		}
	}

	// 关掉总开关 → 全员 1 份。
	p.SetPreferExpiring(false)
	count = map[string]int{}
	for _, uid := range p.WeightedAvailableUIDsForModelRealm("m", "") {
		count[uid]++
	}
	for uid, n := range count {
		if n != 1 {
			t.Fatalf("disabled: %s slots=%d want 1", uid, n)
		}
	}
}

// TestExpiryDistribution 分布倾斜：快过期号在同余额对手前应显著多吃流量。
func TestExpiryDistribution(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := expiryTestPool(t, "expiring", "plain")
	p.SetCreditsDetailed("expiring", CreditsSnapshot{Remain: 1000, Total: 1000, Expiring: 1000, Expiring24h: 1000, Expiring72h: 1000, EarliestAt: now.Add(2 * time.Hour), EarliestRemaining: 1000})
	p.SetCreditsDetailed("plain", CreditsSnapshot{Remain: 1000, Total: 1000})
	p.SetRandomSource(lcg(7))

	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		counts[p.PickExcludingForModel(nil, "m").UID]++
	}
	// 无上界时 6:1（86%）；有 4× 上界后 4:1（80%）。取 65% 作下界。
	if share := float64(counts["expiring"]) / 2000; share < 0.65 {
		t.Fatalf("expiring share=%.2f want ≥0.65 (counts=%v)", share, counts)
	}
}

// lcg 测试用确定性随机源。
func lcg(seed uint64) func(int64) int64 {
	state := seed
	return func(n int64) int64 {
		if n <= 0 {
			return 0
		}
		state = state*6364136223846793005 + 1442695040888963407
		return int64((state >> 33) % uint64(n))
	}
}

// TestExpiryBucketsPersistRoundTrip 分档落盘往返：重启后紧迫度分层不失忆，
// 且脏数据（24h > 72h）在加载时被钳成单调。
func TestExpiryBucketsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	now := time.Now()

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", CreditsSnapshot{Remain: 1000, Total: 1000, Expiring: 800, Expiring24h: 300, Expiring72h: 500, EarliestAt: now.Add(5 * time.Hour), EarliestRemaining: 300})
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("u1 missing after reload")
	}
	if st.CreditsExpiring24h != 300 || st.CreditsExpiring72h != 500 {
		t.Fatalf("reloaded buckets=%d/%d want 300/500", st.CreditsExpiring24h, st.CreditsExpiring72h)
	}
}

// TestExpiryHardLayerExclusiveEndToEnd 硬层端到端：命中硬层时，即使别的号余额高得多，
// 也只在濒临作废组里选（层内再按成本分层）。
func TestExpiryHardLayerExclusiveEndToEnd(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := expiryTestPool(t, "rich", "small", "mid")
	p.SetCreditsDetailed("rich", CreditsSnapshot{Remain: 50000, Total: 50000})
	p.SetCreditsDetailed("small", CreditsSnapshot{Remain: 800, Total: 800, Expiring: 400, Expiring24h: 400, Expiring72h: 400, EarliestAt: now.Add(3 * time.Hour), EarliestRemaining: 400})
	p.SetCreditsDetailed("mid", CreditsSnapshot{Remain: 9000, Total: 9000, Expiring: 3000, Expiring24h: 0, Expiring72h: 0, EarliestAt: now.Add(120 * time.Hour), EarliestRemaining: 3000})
	p.SetRandomSource(lcg(11))

	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		counts[p.PickExcludingForModel(nil, "m").UID]++
	}
	if counts["small"] != 300 {
		t.Fatalf("hard layer must be exclusive: counts=%v", counts)
	}
}
