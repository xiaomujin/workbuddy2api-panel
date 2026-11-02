// 到期偏好策略：三档紧迫度（快/中/慢）+ 金额占比因子 + 相对权重上界 +
// 「濒临作废」硬层。集中所有可调参数与判定谓词，供 pick 流水线与会话粘性分配共用。
package pool

import (
	"strconv"
	"time"
)

// 到期档位（越大越急）。0 = 无到期压力，不参与加权。
const (
	expiryTierNone = 0
	expiryTierSlow = 1 // 窗口内到期（expiring_soon，默认 168h）→ SlowFactor
	expiryTierMid  = 2 // 72h 内到期 → MidFactor
	expiryTierFast = 3 // 24h 内到期 → FastFactor
)

// expirySortMinBoost 排序键启用到期优先的最小加权倍数。
//
// 为什么需要它：排序键把"有到期压力"排在"没有"之前，若不加门槛，一个只到期
// 1 分的号也会压过余额上万的号（金额占比因子把它的权重压到近 1，排序键却照旧
// 把它顶到最前）。取 1.5：只有加权真正显著（≥50% 提升）的号才享受排序优先，
// 微不足道的到期量只影响权重、不影响位次。
const expirySortMinBoost = 1.5

// defaultExpiringWindow 默认快过期窗口，与 config pool.expiring_soon 的默认值一致。
const defaultExpiringWindow = 168 * time.Hour

// ExpiryPolicy 到期偏好的全部可调参数（Q7：集中一处 + SetExpiryPolicy 注入点，
// 暂不接 config/面板——这些数值需要在真实流量下校准，先暴露配置面等于把调参负担
// 转移给运维；结构体 + setter 让后续接 config 是纯增量）。
type ExpiryPolicy struct {
	FastWindow time.Duration // 快档窗口（默认 24h）
	MidWindow  time.Duration // 中档窗口（默认 72h）
	FastFactor float64       // 快档系数（默认 6）
	MidFactor  float64       // 中档系数（默认 4）
	SlowFactor float64       // 慢档系数（默认 3，即引入分档前的 ×3）
	// AmountGain 金额占比放大倍数：share = min(1, 窗口内到期/余额 × AmountGain)。
	// 语义是"这个号剩下的钱里有多少马上就要烂掉"，与紧迫度相乘即"急且值得救"。
	AmountGain float64
	// BoostCap 相对上界：被加权的号最终权重不超过「同层基准权重中位数 × BoostCap」。
	// 用相对而非绝对值：绝对上限在大池/小池下不可比。
	BoostCap float64
	// HardMinAmount / HardMinShare 硬层阈值：24h 内到期金额 ≥ HardMinAmount（绝对积分）
	// 且 ≥ 该号余额 × HardMinShare 时，本次选号进入"濒临作废"独占层。
	// 绝对金额表达"值不值得为它牺牲一次免费请求"；比例条件把"大号零散滴水"
	// （每天到期几百分，占余额 1~3%）排除在外——否则硬层会天天触发并把真正
	// 有风险的大块号挤出候选（实测数据：滴水号 24h 到期 210/11732=1.8%）。
	HardMinAmount int64
	HardMinShare  float64
}

// DefaultExpiryPolicy 默认策略（按实测号池校准，见 README「选号策略」）。
func DefaultExpiryPolicy() ExpiryPolicy {
	return ExpiryPolicy{
		FastWindow:    24 * time.Hour,
		MidWindow:     72 * time.Hour,
		FastFactor:    6,
		MidFactor:     4,
		SlowFactor:    3,
		AmountGain:    2,
		BoostCap:      4,
		HardMinAmount: 200,
		HardMinShare:  0.20,
	}
}

// normalized 修正非法值（窗口/系数 ≤0 视为未设置，回落默认），保证策略始终可用。
func (pol ExpiryPolicy) normalized() ExpiryPolicy {
	def := DefaultExpiryPolicy()
	if pol.FastWindow <= 0 {
		pol.FastWindow = def.FastWindow
	}
	if pol.MidWindow <= 0 {
		pol.MidWindow = def.MidWindow
	}
	if pol.FastFactor <= 0 {
		pol.FastFactor = def.FastFactor
	}
	if pol.MidFactor <= 0 {
		pol.MidFactor = def.MidFactor
	}
	if pol.SlowFactor <= 0 {
		pol.SlowFactor = def.SlowFactor
	}
	if pol.AmountGain <= 0 {
		pol.AmountGain = def.AmountGain
	}
	if pol.BoostCap <= 0 {
		pol.BoostCap = def.BoostCap
	}
	if pol.HardMinAmount < 0 {
		pol.HardMinAmount = 0
	}
	if pol.HardMinShare <= 0 || pol.HardMinShare > 1 {
		pol.HardMinShare = def.HardMinShare
	}
	return pol
}

// factor 返回档位对应的基准系数。
func (pol ExpiryPolicy) factor(tier int) float64 {
	switch tier {
	case expiryTierFast:
		return pol.FastFactor
	case expiryTierMid:
		return pol.MidFactor
	case expiryTierSlow:
		return pol.SlowFactor
	}
	return 1
}

// expiryTierLocked 判定账号当前的到期档位与该档金额。调用方需已持 p.mu。
//
// 档位来源两级：
//  1. 快照分档（creditsExpiring24h / creditsExpiring72h）——余额刷新时烘焙；
//  2. 实时校正：最早批次的距今天数直接决定"至少是哪一档"。快照最长滞后一个刷新
//     周期（默认 5min），但 balance_refresh 关掉的部署只在签到时刷新（最长 12h），
//     没有这层校正，滴水号会在"最后 24h"里整整半天拿不到快档。
//
// 三档只在 expiringWindow ≥ MidWindow 时细分；窗口更窄时退化为慢档单档——否则
// 固定 72h 桶会把窗口外（配置上不该关心）的积分算进中档，自相矛盾。
func (p *Pool) expiryTierLocked(e *entry, now time.Time) (tier int, amount int64) {
	if e.creditsExpiring <= 0 {
		return expiryTierNone, 0
	}
	pol := p.expiryPol
	if p.expiringWindow >= pol.MidWindow {
		switch {
		case e.creditsExpiring24h > 0:
			tier, amount = expiryTierFast, e.creditsExpiring24h
		case e.creditsExpiring72h > 0:
			tier, amount = expiryTierMid, e.creditsExpiring72h
		}
	}
	// 实时校正：只看最早一批的距今时长，把档位往上抬（不抬金额）。
	if e.creditsEarliestRemaining > 0 && !e.creditsEarliestExpiry.IsZero() && now.Before(e.creditsEarliestExpiry) {
		d := e.creditsEarliestExpiry.Sub(now)
		switch {
		case d <= pol.FastWindow && p.expiringWindow >= pol.FastWindow:
			if expiryTierFast > tier {
				tier = expiryTierFast
			}
		case d <= pol.MidWindow && p.expiringWindow >= pol.MidWindow:
			if expiryTierMid > tier {
				tier = expiryTierMid
			}
		}
	}
	if tier == expiryTierNone {
		tier = expiryTierSlow
	}
	// 金额：该档金额不应超过配置窗口内的总额（窗口比档位窄时以窗口为准）。
	if amount <= 0 || amount > e.creditsExpiring {
		amount = e.creditsExpiring
	}
	return tier, amount
}

// expiryBoostLocked 计算到期加权因子与档位（1 = 不加权）。调用方需已持 p.mu。
//
// 闸门（Q2）：只有「本次请求在该号上确实会扣积分」时才加权。免费模型上优先快过期
// 号既不消耗快过期积分，还把免费号挤出候选，纯亏损。
func (p *Pool) expiryBoostLocked(e *entry, model string, now time.Time) (tier int, factor float64) {
	if !p.preferExpiring || p.expiringWindow <= 0 {
		return expiryTierNone, 1
	}
	if !p.entryCostsCredits(e, model, now) {
		return expiryTierNone, 1
	}
	tier, _ = p.expiryTierLocked(e, now)
	if tier == expiryTierNone {
		return expiryTierNone, 1
	}
	pol := p.expiryPol
	share := 1.0
	if e.credits > 0 {
		share = float64(e.creditsExpiring) / float64(e.credits) * pol.AmountGain
		if share > 1 {
			share = 1
		}
	}
	return tier, pol.factor(tier) * share
}

// hardExpiringLocked 报告账号是否"濒临作废"（触发独占硬层）。调用方需已持 p.mu。
//
// 判据：24h 内到期金额 ≥ HardMinAmount 且 ≥ 该号余额 × HardMinShare，且该号对本次
// 请求确实收费。金额取「快照 24h 桶」与「最早批次实时剩余」的较大者，与档位校正
// 同源，避免快照滞后把硬层整段漏掉。
func (p *Pool) hardExpiringLocked(e *entry, model string, now time.Time) bool {
	if !p.preferExpiring || p.expiringWindow <= 0 {
		return false
	}
	pol := p.expiryPol
	if pol.HardMinAmount <= 0 {
		return false
	}
	amt := e.creditsExpiring24h
	if e.creditsEarliestRemaining > amt && !e.creditsEarliestExpiry.IsZero() && now.Before(e.creditsEarliestExpiry) {
		if e.creditsEarliestExpiry.Sub(now) <= pol.FastWindow {
			amt = e.creditsEarliestRemaining
		}
	}
	if amt < pol.HardMinAmount {
		return false
	}
	if !p.entryCostsCredits(e, model, now) {
		return false
	}
	if e.credits <= 0 {
		return true
	}
	return float64(amt)/float64(e.credits) >= pol.HardMinShare
}

// entryCostsCredits 报告"这次请求在该号上是否会扣积分"（到期偏好与硬层的共用闸门）。
//
// 判据与积分保底同源（两级证据）：
//  1. 本地实测台账：有观测就按观测判（tier 0 免费 → false，tier 2 收费 → true）；
//  2. 无观测时用上游目录倍率兜底：倍率 > 0 视为收费，倍率为空/0/非数值视为不收费。
//
// 无观测且目录未覆盖的模型按"不收费"处理（保守方向）：这类模型多为内部/别名模型，
// 给它加权只会把快过期号浪费在无法确认的消耗上。
func (p *Pool) entryCostsCredits(e *entry, model string, now time.Time) bool {
	if model == "" {
		return false
	}
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k > 0
	}
	if p.modelRateOf == nil {
		return false
	}
	rate := p.modelRateOf(e.a.Realm(), model)
	if rate == "" {
		return false
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return false
	}
	return v > 0
}
