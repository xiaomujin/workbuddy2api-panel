package server

import (
	"sync"
	"time"
)

// degradeGate 降级状态机（11-128 内容/渠道拦截的最后一级）。
//
// 历史问题（本次重构的动机）：早先的实现是**进程级单例**——until 只有一份，
// 一次 400 就把「今天所有请求」都换成中性提示词到次日 00:00 CST。后果是：
// 一个客户端（例如系统提示里带 DSH @ 路径段的那个）的指纹，污染全站所有请求；
// 修好之后也回不来（要重启进程）；面板上只看到一个 degrade_active，看不出
// 到底是谁、哪一段、影响了多少请求。
//
// 现在的模型：
//   - **按段作用**：key = (realm, seg)，seg 是 system 文本指纹（prompt.SystemDigest）。
//     只有带同一段的请求受影响，别的请求照旧走 L0/L1。
//   - **短 TTL**：默认 30 分钟（可配），到期自动恢复；不再锁到次日。
//   - **realm 全局兜底**：同一 realm 在 15 分钟内有 ≥N 个**不同段**都走到全阶梯失败，
//     说明不是某一段的问题（可能上游按 realm 封了语义），此时该 realm 整体降级——
//     避免每个请求都白跑 4 次上游调用。这一条是唯一带"全局"语义的路径，且有阈值门槛。
//   - **可清**：面板一键清（Clear），不用重启。
//
// 状态仍是进程内存：重启清零。这是刻意的——降级是"猜测性地换掉出站提示词"，
// 不该跨重启继承一个可能已经过期的判断。
type degradeGate struct {
	mu       sync.Mutex
	entries  map[degradeKey]time.Time
	failures map[string]map[string]time.Time // realm → seg → 首次失败时刻（滑窗用）

	ttl         time.Duration // 段级 TTL；<=0 回退 nextMidnightCST（兼容老行为）
	fallbackTTL time.Duration // realm 全局兜底 TTL；<=0 = 关闭兜底
	threshold   int           // 触发兜底所需的不同段数；<=0 = 关闭兜底
}

// degradeKey 降级作用域。seg 为空串表示「该 realm 的全局兜底」。
type degradeKey struct {
	realm string
	seg   string
}

// DegradeScope 一个降级作用域（/status 与面板展示用）。
type DegradeScope struct {
	Realm string    `json:"realm"`
	Seg   string    `json:"seg"`
	Until time.Time `json:"until"`
}

const (
	defaultDegradeTTL        = 30 * time.Minute
	defaultFallbackTTL       = 30 * time.Minute
	defaultFallbackWindow    = 15 * time.Minute
	defaultFallbackThreshold = 3
)

// configure 设置降级参数（由 NewHandler 从配置注入）。
// ttl<=0 表示"锁到次日 00:00 CST"（老行为逃生门）。
func (g *degradeGate) configure(ttl, fallbackTTL time.Duration, threshold int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ttl = ttl
	g.fallbackTTL = fallbackTTL
	g.threshold = threshold
}

// Active 报告 (realm, seg) 是否处于降级期：段级桶或该 realm 的全局桶任一未到期。
func (g *degradeGate) Active(realm, seg string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.sweepLocked(now)
	if until, ok := g.entries[degradeKey{realm, seg}]; ok && now.Before(until) {
		return true
	}
	if until, ok := g.entries[degradeKey{realm, ""}]; ok && now.Before(until) {
		return true
	}
	return false
}

// ActiveAny 报告该 realm（或任意 realm，realm 为空时）是否有任何作用域在降级期。
func (g *degradeGate) ActiveAny(realm string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.sweepLocked(now)
	for k, until := range g.entries {
		if realm != "" && k.realm != realm {
			continue
		}
		if now.Before(until) {
			return true
		}
	}
	return false
}

// Trigger 触发 (realm, seg) 的降级。
//
// 已在降级期内则不续期（保持最早触发点的到期语义）：反复撞 400 不该把窗口
// 无限往后推——那样一个持续失败的客户端会让恢复时间永远延后。
func (g *degradeGate) Trigger(realm, seg string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.entries == nil {
		g.entries = make(map[degradeKey]time.Time)
	}
	key := degradeKey{realm, seg}
	if until, ok := g.entries[key]; ok && now.Before(until) {
		return
	}
	g.entries[key] = now.Add(g.ttlLocked(now))
}

// NoteFailure 记录一次「该段走完全阶梯仍被拦」，并在同 realm 的不同段达到阈值时
// 触发 realm 全局兜底。返回是否刚刚触发了兜底（调用方据此打日志）。
func (g *degradeGate) NoteFailure(realm, seg string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.threshold <= 0 || g.fallbackTTL <= 0 {
		return false
	}
	if g.failures == nil {
		g.failures = make(map[string]map[string]time.Time)
	}
	bucket := g.failures[realm]
	if bucket == nil {
		bucket = make(map[string]time.Time)
		g.failures[realm] = bucket
	}
	// 滑窗：先清掉窗外记录，再登记本段。
	for s, at := range bucket {
		if now.Sub(at) > defaultFallbackWindow {
			delete(bucket, s)
		}
	}
	if seg == "" {
		return false // 无段指纹的请求不参与"不同段"计数
	}
	if _, ok := bucket[seg]; !ok {
		bucket[seg] = now
	}
	if len(bucket) < g.threshold {
		return false
	}
	// 达到阈值 → realm 全局兜底（同样不续期）。
	if g.entries == nil {
		g.entries = make(map[degradeKey]time.Time)
	}
	key := degradeKey{realm, ""}
	if until, ok := g.entries[key]; ok && now.Before(until) {
		return false
	}
	g.entries[key] = now.Add(g.fallbackTTL)
	delete(g.failures, realm) // 触发后清零计数，避免下一次单段失败又立刻触发
	return true
}

// Snapshot 一次取回「是否降级 + 最晚到期时刻」，供 /status 与面板聚合展示。
//
// 为什么必须能读出来：降级期是**静默改变出站提示词**——客户端那边只表现为
// 「模型变笨了」。不暴露状态的话，这类故障只能靠人猜。
func (g *degradeGate) Snapshot() (bool, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.sweepLocked(now)
	var latest time.Time
	for _, until := range g.entries {
		if now.Before(until) && until.After(latest) {
			latest = until
		}
	}
	if latest.IsZero() {
		return false, time.Time{}
	}
	return true, latest
}

// Scopes 返回当前生效的全部降级作用域（按到期时刻升序无关紧要，面板自行排序）。
func (g *degradeGate) Scopes() []DegradeScope {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.sweepLocked(now)
	out := make([]DegradeScope, 0, len(g.entries))
	for k, until := range g.entries {
		if !now.Before(until) {
			continue
		}
		out = append(out, DegradeScope{Realm: k.realm, Seg: k.seg, Until: until})
	}
	sortScopes(out)
	return out
}

// Clear 清除降级作用域：realm 为空 = 全清；seg 为空且 realm 非空 = 清该 realm
// 的全部作用域（含全局兜底）。面板「一键恢复」与人工灌库后的复位都走它。
func (g *degradeGate) Clear(realm, seg string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for k := range g.entries {
		if realm != "" && k.realm != realm {
			continue
		}
		if seg != "" && k.seg != seg {
			continue
		}
		delete(g.entries, k)
		n++
	}
	if realm == "" {
		g.failures = nil
	} else {
		delete(g.failures, realm)
	}
	return n
}

// ttlLocked 段级 TTL：未配置（<=0）时回退"到次日 00:00 CST"的老行为。
func (g *degradeGate) ttlLocked(now time.Time) time.Duration {
	if g.ttl > 0 {
		return g.ttl
	}
	return nextMidnightCST(now).Sub(now)
}

// sweepLocked 惰性清理过期项与过期失败记录（防止 map 无界增长）。
func (g *degradeGate) sweepLocked(now time.Time) {
	for k, until := range g.entries {
		if !now.Before(until) {
			delete(g.entries, k)
		}
	}
	for realm, bucket := range g.failures {
		for seg, at := range bucket {
			if now.Sub(at) > defaultFallbackWindow {
				delete(bucket, seg)
			}
		}
		if len(bucket) == 0 {
			delete(g.failures, realm)
		}
	}
}

// sortScopes 按 (realm, seg) 稳定排序：面板展示不需要随机顺序（map 迭代是随机的，
// 直接吐出去会让同一状态的两次请求顺序不同，前端 diff 抖动）。
func sortScopes(s []DegradeScope) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0; j-- {
			if s[j-1].realm() <= s[j].realm() {
				break
			}
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// realm 返回排序键（结构体字段私有不便直接拼，用方法表达意图）。
func (d DegradeScope) realm() string { return d.Realm + "\x00" + d.Seg }

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 保留理由：ttl<=0（配 degrade_ttl:"day"）时用它表达"锁到次日"的老语义；
// 同时既有单测（跨月 / 零点边界）一直钉着它，是时区处理唯一的回归锚点。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
