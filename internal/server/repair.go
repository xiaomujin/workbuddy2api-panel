package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// 记忆表的三类输入错误（面板接口直接映射成 400，不做静默忽略——人工灌库写错
// 一个 seg 会静默失效，比报错更难查）。
var (
	errRepairMemoryDisabled = errors.New("repair memory disabled")
	errRepairMemoryKey      = errors.New("repair memory needs realm and seg")
	errRepairMemoryLevel    = errors.New("repair memory level must be 2, 3 or 4")
)

// repairLevel 修复阶梯的级别。
//
// 级别不是随便编号的：数字越大，对客户端 system 提示词的**语义损失越大**，
// 所以阶梯只允许从低到高单向推进，前一级成功就不再进下一级。
//
//	L1 出站前消毒      （internal/upstream/sanitize.go，始终生效，不算"重试级"）
//	L2 拆句            （断连续原文，一字不改、通道仍是 system）
//	L3 搬家            （换承载通道 system → user，一字不改）
//	L4 中性提示词       （丢客户端 prompt，局部作用域 + 短 TTL）
type repairLevel int

const (
	repairNone    repairLevel = 0
	repairSplit   repairLevel = 2
	repairMove    repairLevel = 3
	repairNeutral repairLevel = 4
)

// String 仅用于日志与面板展示。
func (l repairLevel) String() string {
	switch l {
	case repairSplit:
		return "split"
	case repairMove:
		return "move"
	case repairNeutral:
		return "neutral"
	}
	return "none"
}

// RepairConfig 是 repairConfig 的导出别名：cmd/server 要在装配期构造它（字段本身
// 已导出，别名只是让外部包能写下 server.RepairConfig{...} 字面量）。
type RepairConfig = repairConfig

// repairConfig 阶梯配置（由 cmd/server 从 config.json 的 repair 段映射）。
type repairConfig struct {
	Enabled           bool          // 总开关；false = 退回"一次 400 直接降级"的老行为
	EnsureSystemFirst bool          // L0：首条必须是 system（渠道校验硬要求）
	SplitSystem       bool          // L2
	MoveSystemToUser  bool          // L3
	Budget            int           // 每请求允许为修复额外消耗的上游调用数
	DegradeTTL        time.Duration // L4 段级 TTL（0 = 到次日 00:00，兼容老行为）
	FallbackTTL       time.Duration // realm 全局兜底 TTL
	FallbackThreshold int           // 触发兜底所需的不同段数
	MemoryEnabled     bool          // 段指纹记忆（落盘）
	MemoryPath        string        // 记忆文件路径
	PreapplyAfter     int           // 同一段命中多少次后，后续请求直接预置该级
	DumpDir           string        // 全阶梯失败的现场落盘目录（空 = 不落盘，见 repair_dump.go）
}

// 默认值：与 config.example.json 的 repair 段一致，缺省即启用全套阶梯。
const (
	defaultRepairBudget        = 3
	defaultRepairPreapplyAfter = 2
	defaultRepairMemoryPath    = "data/repair-memory.json"
	repairMemoryMaxEntries     = 500
)

// repair 修复阶梯的持有者：状态机 + 段指纹记忆。
//
// 设计约束（来自实际故障）：
//   - 每次变换都从 st.base 派生，不在上一次结果上叠加——旧实现是累积式
//     body = Rewrite(body, …)，三级叠加后互相污染，出问题极难定位。
//   - 记忆 key 必须在**任何修复之前**计算（prompt.SystemDigest(st.base)），
//     否则拆句后 hash 变化，记忆永远命中不了自己写下的条目。
//   - 记不住就没意义：上游加新指纹时靠"同一段第二次直接走上次有效的级别"
//     省掉一次 400 往返（也省掉一次风控暴露）。
type repairLadder struct {
	cfg repairConfig
	mem *repairMemory

	// dumped 已落盘过的 (realm, seg)：同一段只落一份现场。
	// 为什么必须去重：全阶梯失败时每个请求都会走到这里，上游按语义封了某一段时
	// 就是"每个请求写一个文件"——几百个文件换同一段信息，纯属把磁盘写满。
	dumpedMu sync.Mutex
	dumped   map[string]struct{}
}

// newRepair 构建阶梯。cfg 的零值会被补成默认值（Enabled 例外：显式给 true 才启用）。
func newRepair(cfg repairConfig) *repairLadder {
	if cfg.Budget <= 0 {
		cfg.Budget = defaultRepairBudget
	}
	if cfg.PreapplyAfter <= 0 {
		cfg.PreapplyAfter = defaultRepairPreapplyAfter
	}
	r := &repairLadder{cfg: cfg}
	if cfg.MemoryEnabled {
		r.mem = loadRepairMemory(cfg.MemoryPath)
	}
	return r
}

// Enabled 阶梯是否启用（false 时调用方走老的单级降级路径）。
func (r *repairLadder) Enabled() bool { return r != nil && r.cfg.Enabled }

// Budget 返回每请求可用于修复的上游调用预算。
//
// 阶梯关闭时必须返回 0：那个预算会被拿去放大轮转循环上限，禁用状态下多给的
// 迭代次数等于凭空多换几个账号（存量用例 MaxRotate:1 会因此在第二次拿到 200）。
func (r *repairLadder) Budget() int {
	if r == nil || !r.cfg.Enabled || r.cfg.Budget <= 0 {
		return 0
	}
	return r.cfg.Budget
}

// EnsureSystemFirst 是否启用 L0 结构归位。
func (r *repairLadder) EnsureSystemFirst() bool { return r != nil && r.cfg.EnsureSystemFirst }

// Memory 返回记忆表（未启用时为 nil，调用方需判空）。
func (r *repairLadder) Memory() *repairMemory { return r.mem }

// repairState 单个请求的阶梯状态。由 NewState 创建，只在该请求的内存里流转。
type repairState struct {
	base      []byte      // 原始出站 body（EnsureSystemFirst 之后、任何修复之前）
	seg       string      // system 文本指纹（prompt.SystemDigest）
	model     string      // 仅用于记忆条目的可读性
	level     repairLevel // 最近一次生效的级别
	attempted [5]bool     // 已尝试过的级别
	used      int         // 本请求已消耗的修复次数（供 budget 判定）
}

// NewState 为一次请求建状态。body 会被深拷贝：阶梯的每一级都从这份原始 body
// 派生，绝不能引用上游改写管线里可能被就地修改的 map 底层。
func (r *repairLadder) NewState(body []byte, realm, model string) *repairState {
	return r.NewStateWithSeg(body, realm, model, prompt.SystemDigest(body))
}

// NewStateWithSeg 用**外部算好的** seg 建状态。
//
// 为什么需要它：handler 侧必须在网关改写（append / 降级 replace）**之前**就算出
// 客户端 system 指纹——那是"这个请求是谁"的稳定身份。若在这里重算一次 digest，
// 拿到的是改写后的文本，同一客户端会因模式不同落到不同的记忆/降级桶里。
func (r *repairLadder) NewStateWithSeg(body []byte, realm, model, seg string) *repairState {
	base := append([]byte(nil), body...)
	return &repairState{base: base, seg: seg, model: model}
}

// MarkDegraded 标记"本请求的 system 已经换成中性提示词"（预置降级命中时调用），
// 避免阶梯在同一个请求里再把 L4 走一遍。
func (st *repairState) MarkDegraded() {
	if st == nil {
		return
	}
	st.attempted[repairNeutral] = true
	st.level = repairNeutral
}

// Seg 暴露段指纹（日志用）。
func (st *repairState) Seg() string {
	if st == nil {
		return ""
	}
	return st.seg
}

// Level 暴露当前生效级别（日志用）。
func (st *repairState) Level() repairLevel {
	if st == nil {
		return repairNone
	}
	return st.level
}

// Preapply 记忆命中时，在**首次出站之前**就把该级变换套上。
//
// 为什么必须有这一步：Next 只在撞到 400 之后才会被调用，而记忆的价值恰恰是
// "这一段上次在这级成功过，所以这一次不必先撞一次 400"。只记不用等于没记——
// 每个请求仍然要多一次 400 往返（延迟 + 风控暴露都白付）。
func (r *repairLadder) Preapply(st *repairState, realm string) ([]byte, repairLevel, bool) {
	if r == nil || !r.cfg.Enabled || st == nil {
		return nil, repairNone, false
	}
	lvl, ok := r.preapplyLevel(realm, st.seg)
	if !ok || !r.levelEnabled(lvl) {
		return nil, repairNone, false
	}
	out, ok := r.apply(st, lvl)
	if !ok || bytes.Equal(out, st.base) {
		return nil, repairNone, false
	}
	st.attempted[lvl] = true
	st.level = lvl
	return out, lvl, true
}

// Next 返回下一级要重发的 body 与级别；没有可用级别时 ok=false（调用方回内容防火墙错误）。
//
// 顺序固定 L2 → L3 → L4；被配置关掉的级别、已试过的级别跳过。记忆命中时**从该级
// 起步**（跳过更低级），但更高级仍然保留——记忆是"上次在这级成功过"，不是"这级一定行"。
func (r *repairLadder) Next(st *repairState, realm string) ([]byte, repairLevel, bool) {
	if r == nil || !r.cfg.Enabled || st == nil || len(st.base) == 0 {
		return nil, repairNone, false
	}
	if st.used >= r.Budget() {
		return nil, repairNone, false
	}
	var from repairLevel
	if pre, ok := r.preapplyLevel(realm, st.seg); ok {
		from = pre
	}
	for _, lvl := range []repairLevel{repairSplit, repairMove, repairNeutral} {
		if st.attempted[lvl] || lvl < from {
			continue
		}
		if !r.levelEnabled(lvl) {
			continue
		}
		out, ok := r.apply(st, lvl)
		st.attempted[lvl] = true
		if !ok || bytes.Equal(out, st.base) {
			continue // 这一级对当前 body 无计可施，直接试下一级
		}
		st.level = lvl
		st.used++
		return out, lvl, true
	}
	return nil, repairNone, false
}

// apply 生成某一级的 body（始终从 st.base 派生）。
func (r *repairLadder) apply(st *repairState, lvl repairLevel) ([]byte, bool) {
	switch lvl {
	case repairSplit:
		return prompt.SplitSystemMessages(st.base)
	case repairMove:
		return prompt.MoveSystemToUser(st.base, prompt.Degraded)
	case repairNeutral:
		out := prompt.Rewrite(st.base, prompt.Degraded)
		return out, !bytes.Equal(out, st.base)
	}
	return nil, false
}

// levelEnabled 该级是否被配置开启。
func (r *repairLadder) levelEnabled(lvl repairLevel) bool {
	switch lvl {
	case repairSplit:
		return r.cfg.SplitSystem
	case repairMove:
		return r.cfg.MoveSystemToUser
	case repairNeutral:
		return true // L4 是兜底，始终可用（它的 TTL 由 degradeGate 管）
	}
	return false
}

// preapplyLevel 查记忆：同一 (realm, seg) 已连续命中 ≥PreapplyAfter 次时返回记忆级别。
func (r *repairLadder) preapplyLevel(realm, seg string) (repairLevel, bool) {
	if r.mem == nil || seg == "" {
		return repairNone, false
	}
	return r.mem.preapply(realm, seg, r.cfg.PreapplyAfter)
}

// Remember 记录"这一级救了这一段"，供后续请求预置。
func (r *repairLadder) Remember(st *repairState, realm string, lvl repairLevel) {
	if r == nil || r.mem == nil || st == nil || st.seg == "" || lvl == repairNone {
		return
	}
	r.mem.record(realm, st.seg, st.model, lvl)
}

// ── 段指纹记忆 ────────────────────────────────────────────────────────────

// repairMemoryEntry 一条记忆：同一 (realm, seg) 上次在哪一级救回来的。
type repairMemoryEntry struct {
	Realm  string    `json:"realm"`
	Seg    string    `json:"seg"`
	Model  string    `json:"model,omitempty"`
	Level  int       `json:"level"`
	Hits   int       `json:"hits"`
	Source string    `json:"source"` // auto（自动写入）/ manual（人工灌库）
	Note   string    `json:"note,omitempty"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
}

// repairMemoryFile 落盘格式（版本号留着以后迁移用）。
type repairMemoryFile struct {
	Version int                 `json:"version"`
	Entries []repairMemoryEntry `json:"entries"`
}

// repairMemory 段指纹记忆（并发安全，落盘采用临时文件 + rename 的原子替换）。
//
// 为什么不放进 pool 的 state.json：这是**可丢弃的启发式缓存**，丢了只影响一次
// 400 往返；混进账号池状态会让"状态迁移"与"缓存"共用一条持久化路径与迁移逻辑。
type repairMemory struct {
	mu      sync.Mutex
	path    string
	entries map[string]repairMemoryEntry // key = realm + "\x00" + seg
}

func repairMemKey(realm, seg string) string { return realm + "\x00" + seg }

// loadRepairMemory 读取记忆文件。文件不存在 / 读失败 / 格式坏 → 空表（不影响启动：
// 记忆只是优化，缺了它请求照样能靠阶梯救回来）。
func loadRepairMemory(path string) *repairMemory {
	m := &repairMemory{path: path, entries: make(map[string]repairMemoryEntry)}
	if path == "" {
		return m
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var f repairMemoryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return m
	}
	for _, e := range f.Entries {
		if e.Realm == "" || e.Seg == "" {
			continue
		}
		m.entries[repairMemKey(e.Realm, e.Seg)] = e
	}
	return m
}

// preapply 查表：命中且 Hits ≥ minHits 时返回记忆级别。
func (m *repairMemory) preapply(realm, seg string, minHits int) (repairLevel, bool) {
	if m == nil {
		return repairNone, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[repairMemKey(realm, seg)]
	if !ok || e.Hits < minHits || e.Level <= 0 {
		return repairNone, false
	}
	return repairLevel(e.Level), true
}

// record 记录一次成功修复（Hits 累加，Level 以最近一次成功为准）。
func (m *repairMemory) record(realm, seg, model string, lvl repairLevel) {
	if m == nil || seg == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := repairMemKey(realm, seg)
	now := time.Now().UTC()
	e, ok := m.entries[key]
	if !ok {
		e = repairMemoryEntry{Realm: realm, Seg: seg, First: now, Source: "auto"}
	}
	e.Level = int(lvl)
	e.Hits++
	e.Last = now
	if model != "" {
		e.Model = model
	}
	m.entries[key] = e
	m.evictLocked()
	m.saveLocked()
}

// UpsertManual 人工灌库（面板 / 手改文件）：source 固定为 manual，Hits 提到预置阈值，
// 让下一次请求就直接走这一级，而不是再撞一次 400。
func (m *repairMemory) UpsertManual(realm, seg, model, note string, lvl repairLevel) error {
	if m == nil {
		return errRepairMemoryDisabled
	}
	if realm == "" || seg == "" {
		return errRepairMemoryKey
	}
	if lvl < repairSplit || lvl > repairNeutral {
		return errRepairMemoryLevel
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	key := repairMemKey(realm, seg)
	e, ok := m.entries[key]
	if !ok {
		e = repairMemoryEntry{Realm: realm, Seg: seg, First: now}
	}
	e.Level = int(lvl)
	e.Source = "manual"
	e.Note = note
	if model != "" {
		e.Model = model
	}
	if e.Hits < defaultRepairPreapplyAfter {
		e.Hits = defaultRepairPreapplyAfter
	}
	e.Last = now
	m.entries[key] = e
	m.evictLocked()
	m.saveLocked()
	return nil
}

// Remove 删除一条记忆（面板）。
func (m *repairMemory) Remove(realm, seg string) error {
	if m == nil {
		return errRepairMemoryDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, repairMemKey(realm, seg))
	m.saveLocked()
	return nil
}

// Snapshot 返回按最近命中排序的记忆副本（面板展示）。
func (m *repairMemory) Snapshot() []repairMemoryEntry {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]repairMemoryEntry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// evictLocked 上限保护：超过 repairMemoryMaxEntries 时按 Last 淘汰最旧的。
func (m *repairMemory) evictLocked() {
	if len(m.entries) <= repairMemoryMaxEntries {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(m.entries))
	for k, e := range m.entries {
		all = append(all, kv{k, e.Last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for i := 0; i < len(all)-repairMemoryMaxEntries; i++ {
		delete(m.entries, all[i].k)
	}
}

// saveLocked 原子落盘：先写临时文件再 rename（避免半截 JSON 让下次启动静默丢表）。
func (m *repairMemory) saveLocked() {
	if m.path == "" {
		return
	}
	f := repairMemoryFile{Version: 1, Entries: make([]repairMemoryEntry, 0, len(m.entries))}
	for _, e := range m.entries {
		f.Entries = append(f.Entries, e)
	}
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Last.After(f.Entries[j].Last) })
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(m.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.path)
}
