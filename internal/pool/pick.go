// 选号：Pick 簇（healthy 成本分层 + 快过期虚拟实例权重 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// pickTopK 短名单长度：排序后截断到此长度再抽签。
const pickTopK = 5

// pickCtx 一次选号的只读上下文，供流水线各段共用（避免把 now/tried/model/realm
// 逐层透传，也避免各段各自 time.Now() 造成同一轮内时刻漂移）。
type pickCtx struct {
	now   time.Time
	tried map[string]bool
	model string
	realm string
}

// pick 选号流水线：候选过滤 → 分层排序 → 抽签 → 兜底 → 收口。
// 每段都是持锁调用的纯逻辑（只读 p/e 状态、只写探索台账），便于单测与等价性比对。
//
// 模型感知：reqModel 非空时健康口径换成 healthyForModel（6004 模型豁免生效）；
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := pickCtx{now: time.Now(), tried: tried, model: reqModel, realm: realm}
	cands := p.candidatesLocked(ctx)
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, ctx.now, realm, reqModel)
	}
	ranked, explored := p.rankLocked(ctx, cands)
	top := ranked
	if len(top) > pickTopK {
		top = top[:pickTopK]
	}
	e := p.sampleLocked(ctx, top, ranked)
	if explored {
		// 探索事件日志（可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			ctx.model, ctx.realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	p.commitLocked(e, ctx.now)
	return e.a
}

// candidatesLocked 候选过滤：tried / 账号级健康 / 模型级健康 / realm / 积分保底 /
// 在途占满，六条并列谓词全过才入候选。
//
// 惰性清理：过期的模型级冷却与成本台账在此真正删除（两者的 map 都不无限膨胀；
// status 只读遍历天然跳过过期项，但内存条目必须有回收点）。
func (p *Pool) candidatesLocked(ctx pickCtx) []*entry {
	realmOK := func(e *entry) bool { return ctx.realm == "" || e.a.Realm() == ctx.realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(ctx.now) }
	if ctx.model != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(ctx.now, ctx.model) }
	}
	// floorBlocked 积分保底拦截判定（实现在 floorBlockedForRealmModel，与粘性路径共用）：
	// 触底 + 收费（本地实测台账 或 上游目录倍率）即拦；免费/未知倍率不受限。
	floorBlocked := func(e *entry) bool { return p.floorBlockedForRealmModel(e, ctx.model, ctx.realm, ctx.now) }
	var cands []*entry
	for uid, e := range p.byUID {
		if ctx.tried != nil && ctx.tried[uid] {
			continue
		}
		e.pruneExpiredModelCooldowns(ctx.now)
		e.pruneExpiredModelCosts(ctx.now)
		if !healthyOf(e) {
			continue
		}
		if floorBlocked(e) {
			continue // 积分保底：触底号不接实测收费模型（tier 0/1 不受限）
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	return cands
}

// scored 单个候选的评分结果：成本层 + 权重 + 到期档位。
// 权重只算一次、在排序与抽签两个阶段共用（旧实现两阶段各自重算 maxCredits 基准，
// 导致"权重可比"的注释与实际不符）。
type scored struct {
	e      *entry
	w      float64 // 最终权重（含到期乘子，已按相对上界钳制）
	base   float64 // 基准权重（余额 + 闲置），相对上界的比较基准
	tier   int     // 成本层（0 免费 / 1 无观测 / 2 收费）
	exp    int     // 到期档位（0 = 不参与到期优先位次）
	boost  float64 // 到期乘子（1 = 未加权）
	cost1k float64
}

// rankLocked 分层 + 打分 + 排序，返回全部候选（降序）与「本次是否切了探索层」。
//
// 分层顺序（由外到内）：到期硬层 → 成本层 → 权重。硬层在外是刻意的：免费号明天
// 还在，快过期的积分明天就没了——这是整个系统里唯一"钱会物理消失"的场景。
//
// 成本分层（model 非空时）：按该模型的实测扣费把候选分层，只保留最优层。
//
//	0 = 已实测免费（限免期/夜间免费的号，最强偏好）
//	1 = 无观测（含观测过期）
//	2 = 已实测收费
//
// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
// 为什么用硬过滤而非仅排序：抽签会在候选内加权随机，只排序的话收费号仍有机会抽中。
func (p *Pool) rankLocked(ctx pickCtx, cands []*entry) ([]scored, bool) {
	// maxCredits 统一用**全集口径**（任何过滤前的全部 healthy 候选）：截断排序与
	// 抽签权重共享同一基准。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(ctx.model, ctx.now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	// 到期硬层：把"24h 内要烂掉一大块"的号挑出来独占候选。
	// 组为空（被 tried/冷却/在途过滤干净）时不启用，回落普通路径——保险丝的作用
	// 是"优先烧掉要烂的钱"，不是"烧不掉就拒绝服务"。
	hard := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if p.hardExpiringLocked(e, ctx.model, ctx.now) {
			hard = append(hard, e)
		}
	}
	hardActive := len(hard) > 0
	if hardActive {
		cands = hard
	}
	bestTier := 2
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	explored := false // 本次 pick 是否切了探索层（事件日志在选中号确定后打）
	// 条件探索（issue #136 方案 a′）：tier 0 垄断层存在（bestTier==0 且 model
	// 非空）且候选含 tier 1（冻结存在）且距上次探索 ≥ 窗口（零值 timer=从未探索
	// →首次满足即探）时，本次 pick 生效层切 tier 1-only——探索=搭车改道，把一个
	// 既有真实用户请求改道给未知号（零新增上游请求；IP 维度零增量，WAF 友好）。
	// 成功 → NoteModelCost 首观测 → 毕业（tier 0/2，下一轮 pick 立即生效）；
	// 失败 → 既有错误策略照常，无探测风暴。
	// 硬层激活时**跳过探索且不推进 timer**：两者目的相反（硬层要把钱烧在快过期号上，
	// 探索要把请求改道给未知号），推进 timer 会白耗掉这个模型的探索窗口。
	// timer 同锁写入：并发 pick 串行进入写锁，只有一个进入者能通过窗口判定
	//（天然防重复探索）。key = realm + "\x1f" + model：同模型名可跨域，
	// 探索节奏按 (域, 模型) 独立；realm==""（Pick 老语义）单独成键。
	if !hardActive && p.costExploreInterval > 0 && bestTier == 0 && ctx.model != "" {
		hasTier1 := false
		for _, e := range cands {
			if ti, _ := costTier(e); ti == 1 {
				hasTier1 = true
				break
			}
		}
		key := ctx.realm + "\x1f" + ctx.model
		if hasTier1 && ctx.now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = ctx.now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	// 打分：基准权重（余额 + 闲置）与到期乘子各算一次，比较器只读缓存字段。
	out := make([]scored, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti != bestTier {
			continue
		}
		base := p.weightOf(e, maxCredits, ctx.now)
		exp, boost := p.expiryBoostLocked(e, ctx.model, ctx.now)
		out = append(out, scored{e: e, w: base * boost, base: base, tier: ti, exp: exp, boost: boost, cost1k: ci})
	}
	p.capExpiryBoostLocked(out)
	// 排序键门槛：加权不足 expirySortMinBoost 的号不享受"到期优先"位次（只影响
	// 权重、不影响顺序），避免"只到期几分"的号压过余额上万的号。
	for i := range out {
		if out[i].boost < expirySortMinBoost {
			out[i].exp = expiryTierNone
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		// 到期档位优先：保证 top5 一定包含最紧迫的号（只靠权重乘子的话，一个紧迫
		// 但余额很低的号可能仍进不了短名单，硬层之外的软偏好就白设了）。
		if out[i].exp != out[j].exp {
			return out[i].exp > out[j].exp
		}
		if out[i].cost1k != out[j].cost1k {
			return out[i].cost1k < out[j].cost1k // 收费层：单价低的在前
		}
		if out[i].w != out[j].w {
			return out[i].w > out[j].w
		}
		return out[i].e.a.UID < out[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	if len(out) > pickTopK {
		p.shuffleEqualRunsLocked(out, ctx.now)
	}
	return out, explored
}

// capExpiryBoostLocked 给到期加权设相对上界：被加权的号最终权重不超过
// 「同层基准权重中位数 × BoostCap」。
//
// 中位数取**同层全部候选的基准权重**（不含到期乘子）：若把被加权的号也算进去，
// top5 全是快过期号时中位数被抬高，等于没设上界。上界只作用于 boost>1 的号，
// 不碰其它号的既有分布（否则一次"加到期偏好"的改动会扩成全局调参）。
func (p *Pool) capExpiryBoostLocked(ws []scored) {
	if len(ws) == 0 {
		return
	}
	need := false
	for i := range ws {
		if ws[i].boost > 1 {
			need = true
			break
		}
	}
	if !need {
		return
	}
	bases := make([]float64, len(ws))
	for i := range ws {
		bases[i] = ws[i].base
	}
	sort.Float64s(bases)
	median := bases[len(bases)/2]
	if median <= 0 {
		return
	}
	limit := median * p.expiryPol.BoostCap
	for i := range ws {
		if ws[i].boost > 1 && ws[i].w > limit {
			ws[i].w = limit
		}
	}
}

// shuffleEqualRunsLocked 在**相等权重段内**做 Fisher-Yates 洗牌（截断边界随机化）。
//
// 修复旧实现的判定缺陷：旧代码只比较 ws[i].w == ws[0].w，若 ws[0] 是唯一最大值、
// 其余全体并列，则不洗牌 → 并列者按 uid 字典序被截断，正是这段逻辑想消灭的
// 「字典序饿死」。这里按 (档位, 单价, 权重) 三元组识别并列段，段内洗牌。
//
// 洗牌用独立的 time-seeded 源，**不消耗 p.randInt64N 注入源**——加权抽签的确定性
// 语义（TestPickDeterministicViaSetRandomSource）保持不变。
func (p *Pool) shuffleEqualRunsLocked(ws []scored, now time.Time) {
	shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
	for i := 0; i < len(ws); {
		j := i + 1
		for j < len(ws) && ws[j].exp == ws[i].exp && ws[j].cost1k == ws[i].cost1k && ws[j].w == ws[i].w {
			j++
		}
		if j-i > 1 {
			seg := j - i
			shuf.Shuffle(seg, func(a, b int) { ws[i+a], ws[i+b] = ws[i+b], ws[i+a] })
		}
		i = j
	}
}

// sampleLocked 在 top5 内抽签；top5 全部刚被用过时回落到全候选的加权 LRU。
//
// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入。
func (p *Pool) sampleLocked(ctx pickCtx, top, ranked []scored) *entry {
	eligible := make([]scored, 0, len(top))
	for _, c := range top {
		if ctx.now.Sub(c.e.lastUsed) >= minPickGap {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) > 0 {
		return p.pickWeighted(eligible).e // eligible 保序 = top5 降序子集
	}
	// top5 全部刚被用过：在**全候选 ranked** 里取 usedSeq 最旧的 K 个，再在它们中间
	// 加权抽签。旧实现直接取最旧的一个——高并发下（>5 并发/100ms）这条路径会成为
	// 主路径，权重与到期偏好全部失效，退化成严格轮转。
	// 用 usedSeq 单调序号而非 lastUsed 墙钟：Windows 等平台 time.Now() 精度 ~0.5ms，
	// 快速连续选号时所有 lastUsed 完全相等，比较会恒选同一个。
	k := pickTopK
	if k > len(ranked) {
		k = len(ranked)
	}
	all := make([]scored, len(ranked))
	copy(all, ranked)
	sort.SliceStable(all, func(i, j int) bool { return all[i].e.usedSeq < all[j].e.usedSeq })
	return p.pickWeighted(all[:k]).e
}

// commitLocked 选中收口：记 lastUsed（防并发撞号）与单调 usedSeq（LRU 权威依据）。
func (p *Pool) commitLocked(e *entry, now time.Time) {
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
}

// floorBlockedForModel 积分保底拦截判定（pick 普通轮换与 PickByUIDForModel 粘性
// 路径的单一事实来源）：floor>0 且账号触底（credits < floor）且该模型**收费**
// 时为真。
//
// 收费判据两级（任一成立即判收费 → 拦）：
//  1. 本地实测台账（e.modelCostOf）：该号在该模型上实测 cost>0（tier 2）。
//  2. 上游目录倍率（p.modelRateOf）：本地无观测/观测过期（tier 1）时的兜底。
//     只看实测会让「无观测」恒等于「放行」——而高价新模型恰恰全池无观测
//     （kimi-k3-1 实案：x1.62、223 分/百万 token，两笔打穿 100 分的号并触发
//     硬冷却到次日 04:00）。倍率由上游随模型目录下发，请求前即已知，不必付学费。
//
// 不拦的情形：
//   - 模型免费：本地实测 cost<=0（tier 0），或目录倍率为 0/"0.00"。保底的目的
//     正是「留余额给免费模型用」——但若账号已归零，上游仍会 402（余额门禁是
//     账号级的，与模型无关），此时由 ErrHardCredit 冷却承接，与本判定无关。
//   - 模型倍率未知（台账无观测且目录未下发该模型）：无法判收费，按放行处理。
//     这是有意的保守选择——目录未覆盖的模型多为内部/别名模型，拦了会让号
//     永久失联；风险由「未知」本身承担，但已知收费的一律拦。
//   - model 为空（无模型上下文）不拦：无成本维度，floor 无从判收费。
//
// 余额用本地插值口径（签到权威值 - 每笔 usage.credit 实扣，见 NoteModelCost）：
// 只会偏低不会偏高（官方对账延迟方向安全），正是保底需要的安全方向。
// 调用方必须已持有 p.mu（读 e.credits / e.modelCost / p.modelRateOf）。
func (p *Pool) floorBlockedForModel(e *entry, model string, now time.Time) bool {
	return p.floorBlockedForRealmModel(e, model, "", now)
}

// floorBlockedForRealmModel 同上，但带 realm 上下文（倍率按 (realm, 模型) 分桶，
// 同名模型在 CN / global 两域倍率可不同）。realm 为空时按倍率表的空域键查。
func (p *Pool) floorBlockedForRealmModel(e *entry, model, realm string, now time.Time) bool {
	if p.creditFloor <= 0 || model == "" || e.credits >= p.creditFloor {
		return false
	}
	// 1) 本地实测台账：最权威（真实扣费证据）。
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k > 0
	}
	// 2) 上游目录倍率兜底：无实测观测时用牌价判收费，堵住「无观测 = 放行」漏洞。
	if p.modelRateOf == nil {
		return false
	}
	rate := p.modelRateOf(realm, model)
	if rate == "" {
		return false // 目录未覆盖：未知，放行（见上方注释）
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return false // 倍率非数值（异常形态）：不据此惩罚账号
	}
	return v > 0
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
//
// 积分保底同样在此生效（model 非空时）：floor 把健康号全部拦掉后 cands 为空会走到
// 这里，若兜底不看保底，触底号会被「捞回来」继续接收费模型——表现为同一条
// floor WARN 反复刷同一个号（实测：credits=1 < floor=150 仍持续中选）。
// 兜底是**最后一道**选号路径，保底在它之前挡不住就等于没挡。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, model string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled || e.paused {
			continue // 禁用/暂停选号的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue // 积分保底：触底号不接收费模型（兜底路径同判据）
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（上限按 realm 分档，见 inFlightLimit；
// limit=0 不限 → 恒 false）。调用方需已持 p.mu（读锁或写锁均可，本方法只读上限）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 加权随机（claude-api selectWeightedRandom 参考口径）。
//
// 权重由调用方预计算（scored.w，已含到期乘子与相对上界），本函数只做定点抽签：
// 两个阶段共用同一权重来源，避免"排序用全集基准、抽签用子集基准"的不一致。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []scored) *scored {
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i := range cands {
		w := cands[i].w
		if w < 0 {
			w = 0
		}
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return &cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i := range cands {
		acc += weights[i]
		if r < acc {
			return &cands[i]
		}
	}
	return &cands[len(cands)-1]
}

// weightOf 计算单个账号的普通加权分值。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3.（原「成功率 ×3」因子已删，对齐上游 success-ema-review：errTotal 是终身
	// 累计、只增不减，成功率 = successCount/(successCount+errTotal) 会让早期出过错
	// 的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。）
	return w
}

// SetCredits 更新账号余额。
