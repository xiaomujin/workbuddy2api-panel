// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快档（24h 内到期）账号在新会话候选集中的虚拟实例数上限。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// expiringSlotsForTier 到期档位 → 虚拟实例数（Q20a：24h→3、72h/窗口→2、无→1）。
//
// 为什么不做成"按软权重折算 1~3 份"：浮点权重映射成整数份数会放大分配方差，
// 且同一账号拓扑下不同进程的哈希结果会不一致（破坏"同拓扑同结果"的稳定性）。
func expiringSlotsForTier(tier int) int {
	switch tier {
	case expiryTierFast:
		return expiringVirtualSlots
	case expiryTierMid, expiryTierSlow:
		return 2
	}
	return 1
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；快过期账号按档位出现 2~expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		// 闸门与 pick 同口径：只有"本次请求在该号上会扣积分"时才给虚拟实例，
		// 否则免费模型上会把快过期号顶到新会话前面，白白挤掉免费号。
		if p.preferExpiring && p.expiringWindow > 0 {
			if e := p.byUID[uid]; p.entryCostsCredits(e, model, now) {
				tier, _ := p.expiryTierLocked(e, now)
				slots = expiringSlotsForTier(tier)
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
