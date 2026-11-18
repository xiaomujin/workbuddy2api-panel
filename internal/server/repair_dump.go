package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 修复现场的落盘诊断（对应"人工灌库"工作流的第一半：先记录，再离线分析）。
//
// 为什么不落**完整请求体**：指纹段都在 system 里，user / assistant 内容是用户的
// 真实数据（可能含代码、路径、凭证），把它写进磁盘换一个"能定位触发段"的能力不值得。
// 这里只写 system 文本、消息角色序列与段指纹——足够在离线工具里二分定位，也不把
// 对话内容带出网关。
//
// 触发时机：**全阶梯失败**（即将回内容防火墙错误）时才落一份。成功救回的请求不落盘
// ——那是常态，落了只会淹没真正的现场。

// repairDump 落盘格式。版本号留着以后扩展字段用。
type repairDump struct {
	Version     int       `json:"version"`
	Time        time.Time `json:"time"`
	Realm       string    `json:"realm"`
	Seg         string    `json:"seg"`
	Model       string    `json:"model"`
	Reason      string    `json:"reason"`
	SystemTexts []string  `json:"system_texts"`
	Roles       []string  `json:"roles"`
	BodyBytes   int       `json:"body_bytes"`
}

// repairDumpMaxSegs 进程生命周期内最多为多少个**不同段**落盘。
// 去重已把"同一段反复写"挡掉；这个上限挡的是"上游按语义封了一大片、每段都不同"
// 的极端情况（否则磁盘上没有自然的停止条件）。
const repairDumpMaxSegs = 200

// Dump 把一次无法修复的拦截现场写到 DumpDir（未配置时是 no-op）。
//
// 同一 (realm, seg) 只写一份：拦截是持续状态，一次复现就够离线分析，重复写只会
// 把磁盘写满而信息量为零。
//
// 返回写出的文件路径（未写时为空串），便于调用方打日志。
func (r *repairLadder) Dump(st *repairState, realm, model, reason string) string {
	if r == nil || r.cfg.DumpDir == "" || st == nil || len(st.base) == 0 {
		return ""
	}
	key := realm + "\x00" + st.seg
	r.dumpedMu.Lock()
	if _, seen := r.dumped[key]; seen || len(r.dumped) >= repairDumpMaxSegs {
		r.dumpedMu.Unlock()
		return ""
	}
	r.dumpedMu.Unlock()
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(st.base, &obj); err != nil {
		return ""
	}
	d := repairDump{
		Version:   1,
		Time:      time.Now().UTC(),
		Realm:     realm,
		Seg:       st.seg,
		Model:     model,
		Reason:    reason,
		BodyBytes: len(st.base),
	}
	for _, m := range obj.Messages {
		d.Roles = append(d.Roles, m.Role)
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil && text != "" {
			d.SystemTexts = append(d.SystemTexts, text)
		}
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(r.cfg.DumpDir, 0o755); err != nil {
		return ""
	}
	seg := st.seg
	if seg == "" {
		seg = "noseg"
	}
	name := time.Now().UTC().Format("20060102T150405Z") + "-" + sanitizeFileToken(realm) + "-" + sanitizeFileToken(seg) + ".json"
	path := filepath.Join(r.cfg.DumpDir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return ""
	}
	// 写成功才记账：写失败（权限/磁盘满）不该把这一段永久标成"已落盘"。
	r.dumpedMu.Lock()
	if r.dumped == nil {
		r.dumped = make(map[string]struct{})
	}
	r.dumped[key] = struct{}{}
	r.dumpedMu.Unlock()
	return path
}

// sanitizeFileToken 只保留文件名安全字符（realm/seg 都来自内部，仍然兜一层：
// 文件名拼接是最容易被"上游返回值"污染的地方）。
func sanitizeFileToken(s string) string {
	if s == "" {
		return "x"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
