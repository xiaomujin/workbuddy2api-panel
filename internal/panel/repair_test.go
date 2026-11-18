package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 修复阶梯运维接口的契约：明细可见、能一键清、能人工灌库/删条目。
// 这些接口存在的理由就是"降级与结构级修复都是静默的"——所以断言重点在
// **可读性字段**（realm/seg/until/realm_wide）与**错误必须显式**（不静默吞）。

func newRepairPanel() (*Panel, *[]map[string]any, *[]int, *[]string) {
	scopes := []map[string]any{
		{"realm": "cn", "seg": "a1b2c3d4", "until": "2026-11-18T13:30:00+08:00", "realm_wide": false},
	}
	cleared := []int{}
	added := []string{}
	p := &Panel{cfg: Config{
		DegradeScopes: func() []map[string]any { return scopes },
		DegradeClear: func(realm, seg string) int {
			cleared = append(cleared, len(scopes))
			return 1
		},
		RepairMemory: func() []map[string]any {
			return []map[string]any{{"realm": "cn", "seg": "a1b2c3d4", "level": 2, "hits": 3, "source": "auto"}}
		},
		RepairAdd: func(realm, seg, model, note string, level int) error {
			added = append(added, fmt.Sprintf("%s|%s|%d|%s", realm, seg, level, note))
			if level < 2 || level > 4 {
				return fmt.Errorf("bad level")
			}
			return nil
		},
		RepairRemove: func(realm, seg string) error {
			if realm == "" || seg == "" {
				return fmt.Errorf("missing")
			}
			return nil
		},
	}}
	return p, &scopes, &cleared, &added
}

func TestPanelDegradeScopes(t *testing.T) {
	p, _, _, _ := newRepairPanel()
	rec := httptest.NewRecorder()
	p.degradeScopesHandler(rec, httptest.NewRequest("GET", "/panel/api/degrade/scopes", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var got struct {
		Scopes   []map[string]any `json:"scopes"`
		Total    int              `json:"total"`
		CanClear bool             `json:"can_clear"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.Total != 1 || !got.CanClear {
		t.Fatalf("total=%d can_clear=%v", got.Total, got.CanClear)
	}
	if got.Scopes[0]["seg"] != "a1b2c3d4" {
		t.Fatalf("scopes=%+v", got.Scopes[0])
	}
}

func TestPanelDegradeClear(t *testing.T) {
	p, _, cleared, _ := newRepairPanel()
	rec := httptest.NewRecorder()
	// 空 body = 全清（curl -X POST 不带体的最常见用法）
	p.degradeClearHandler(rec, httptest.NewRequest("POST", "/panel/api/degrade/clear", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*cleared) != 1 {
		t.Fatalf("clear 未被调用")
	}
	// 非法 JSON 必须显式 400（不静默当成全清）
	rec2 := httptest.NewRecorder()
	p.degradeClearHandler(rec2, httptest.NewRequest("POST", "/panel/api/degrade/clear", strings.NewReader("{oops")))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec2.Code)
	}
}

func TestPanelRepairMemoryList(t *testing.T) {
	p, _, _, _ := newRepairPanel()
	rec := httptest.NewRecorder()
	p.repairMemoryHandler(rec, httptest.NewRequest("GET", "/panel/api/repair/memory", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var got struct {
		Total  int              `json:"total"`
		Levels map[string]int   `json:"levels"`
		Entrys []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.Total != 1 || got.Levels["split"] != 2 || got.Levels["neutral"] != 4 {
		t.Fatalf("got=%+v", got)
	}
}

func TestPanelRepairMemoryAdd(t *testing.T) {
	p, _, _, added := newRepairPanel()
	rec := httptest.NewRecorder()
	p.repairMemoryAddHandler(rec, httptest.NewRequest("POST", "/panel/api/repair/memory",
		strings.NewReader(`{"realm":"cn","seg":"a1b2c3d4","level":3,"note":"离线二分：单句即拦"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*added) != 1 || !strings.Contains((*added)[0], "cn|a1b2c3d4|3|") {
		t.Fatalf("added=%v", *added)
	}
	// level 缺省按最保守的结构级（2）灌入
	rec2 := httptest.NewRecorder()
	p.repairMemoryAddHandler(rec2, httptest.NewRequest("POST", "/panel/api/repair/memory",
		strings.NewReader(`{"realm":"cn","seg":"ffffffff"}`)))
	if rec2.Code != 200 {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	if !strings.Contains((*added)[1], "|2|") {
		t.Fatalf("缺省级别应为 2，got %v", (*added)[1])
	}
	// 非法 level 必须 400（写错一个段指纹会静默失效，比报错难查）
	rec3 := httptest.NewRecorder()
	p.repairMemoryAddHandler(rec3, httptest.NewRequest("POST", "/panel/api/repair/memory",
		strings.NewReader(`{"realm":"cn","seg":"x","level":9}`)))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec3.Code)
	}
}

func TestPanelRepairMemoryRemove(t *testing.T) {
	p, _, _, _ := newRepairPanel()
	rec := httptest.NewRecorder()
	p.repairMemoryRemoveHandler(rec, httptest.NewRequest("DELETE", "/panel/api/repair/memory?realm=cn&seg=a1b2c3d4", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	rec2 := httptest.NewRecorder()
	p.repairMemoryRemoveHandler(rec2, httptest.NewRequest("DELETE", "/panel/api/repair/memory?realm=cn", nil))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("缺 seg 应 400，got %d", rec2.Code)
	}
}
