package panel

import (
	"encoding/json"
	"net/http"
	"strings"
)

// 本文件是「11-128 修复阶梯」的面板侧接口：
//
//	GET    /panel/api/degrade/scopes   逐作用域降级明细（realm + 段指纹 + 到期）
//	POST   /panel/api/degrade/clear    一键恢复（按 realm / 按段 / 全清）
//	GET    /panel/api/repair/memory    段指纹记忆清单
//	POST   /panel/api/repair/memory    人工灌库（写一条：realm + seg → 级别）
//	DELETE /panel/api/repair/memory    删除一条记忆
//
// 为什么这些是必要的：降级与结构级修复都是**静默改变出站提示词**——客户端只看
// 到"模型变笨了"。没有面板出口的话，运维只能靠重启进程复位、靠抓包猜是哪一段
// 触发的。这里把"看得见"与"能复位/能灌库"补齐。

// degradeScopesHandler 逐作用域降级明细。
func (p *Panel) degradeScopesHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.DegradeScopes == nil {
		writeErr(w, http.StatusNotImplemented, "degrade scopes not available")
		return
	}
	scopes := p.cfg.DegradeScopes()
	if scopes == nil {
		scopes = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scopes": scopes,
		"total":  len(scopes),
		// 能力标记：前端据此决定是否渲染「一键恢复」按钮（与鉴权/装配无关的纯能力位）。
		"can_clear": p.cfg.DegradeClear != nil,
	})
}

// degradeClearHandler 一键恢复。body 可选 {"realm":"cn","seg":"a1b2c3d4"}；两者都空 = 全清。
func (p *Panel) degradeClearHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.DegradeClear == nil {
		writeErr(w, http.StatusNotImplemented, "degrade clear not available")
		return
	}
	var req struct {
		Realm string `json:"realm"`
		Seg   string `json:"seg"`
	}
	// 空 body 视为全清（curl -X POST 不带体是最常见的用法）。
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
	}
	realm := strings.TrimSpace(req.Realm)
	seg := strings.TrimSpace(req.Seg)
	n := p.cfg.DegradeClear(realm, seg)
	writeJSON(w, http.StatusOK, map[string]any{"cleared": n, "realm": realm, "seg": seg})
}

// repairMemoryHandler 记忆清单。
func (p *Panel) repairMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RepairMemory == nil {
		writeErr(w, http.StatusNotImplemented, "repair memory not available")
		return
	}
	entries := p.cfg.RepairMemory()
	if entries == nil {
		entries = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
		// 级别的语义写在响应里，前端不必硬编码：2=拆句 3=搬家 4=中性提示词。
		"levels": map[string]int{"split": 2, "move": 3, "neutral": 4},
	})
}

// repairMemoryAddHandler 人工灌库：把 (realm, seg) 固定到某一级。
//
// 用途：真机上已经查明"这一段要拆句才过"（或"只能降级"）之后，直接写进来，
// 下一个请求就按该级出站，不必再撞一次 400。level 必须是 2/3/4——写错要报错，
// 不静默忽略（写错一个 seg 的后果是它静默失效，比报错难查得多）。
func (p *Panel) repairMemoryAddHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RepairAdd == nil {
		writeErr(w, http.StatusNotImplemented, "repair memory not available")
		return
	}
	var req struct {
		Realm string `json:"realm"`
		Seg   string `json:"seg"`
		Model string `json:"model"`
		Note  string `json:"note"`
		Level int    `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	req.Realm = strings.TrimSpace(req.Realm)
	req.Seg = strings.TrimSpace(req.Seg)
	if req.Level == 0 {
		req.Level = 2 // 缺省按最保守的结构级修复（一字不改）灌入
	}
	if err := p.cfg.RepairAdd(req.Realm, req.Seg, req.Model, req.Note, req.Level); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// repairMemoryRemoveHandler 删除一条记忆（按 realm + seg）。
func (p *Panel) repairMemoryRemoveHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RepairRemove == nil {
		writeErr(w, http.StatusNotImplemented, "repair memory not available")
		return
	}
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	seg := strings.TrimSpace(r.URL.Query().Get("seg"))
	if realm == "" || seg == "" {
		writeErr(w, http.StatusBadRequest, "realm and seg are required")
		return
	}
	if err := p.cfg.RepairRemove(realm, seg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
