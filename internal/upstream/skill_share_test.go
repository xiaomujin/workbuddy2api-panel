package upstream

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDesktopSkillShareSequence 锁定技能分享埋点链的形状（客户端 asar 逆向口径）：
// 顺序 = detail_show → entry_click → security_result → channel_action(click) →
// channel_action(success)。
func TestDesktopSkillShareSequence(t *testing.T) {
	const id, name, ver = "skill_2097350077599879168", "润泽小馆·日报撰写", "1.0.0"
	events := DesktopSkillShareSequence(id, name, ver)
	wantCodes := []string{
		"skill_detail_show",
		"skill_share_entry_click",
		"skill_share_security_result",
		"skill_share_channel_action",
		"skill_share_channel_action",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if got := events[i]["eventCode"]; got != want {
			t.Errorf("event[%d] code=%v want %s", i, got, want)
		}
		// 版本由公共指纹统一注入（单一真源），业务事件不自带 ideVersion/extVersion。
		if _, ok := events[i]["ideVersion"]; ok {
			t.Errorf("event[%d] 不应自带 ideVersion（改由 desktopFingerprint 注入）", i)
		}
		if _, ok := events[i]["extVersion"]; ok {
			t.Errorf("event[%d] 不应自带 extVersion（改由 desktopFingerprint 注入）", i)
		}
	}
	// detail_show 用 name 字段（客户端 reportSkillDetailShow 口径），分享事件用 skillId。
	if got := events[0]["name"]; got != name {
		t.Errorf("detail_show name=%v want %s", got, name)
	}
	if got := events[1]["skillId"]; got != id {
		t.Errorf("entry_click skillId=%v want %s", got, id)
	}
	if got := events[1]["entry"]; got != "skill_detail" {
		t.Errorf("entry_click entry=%v want skill_detail", got)
	}
	// 渠道动作：click → success 两次，且 isSuccessful=true（判据落在这里，实测点亮）。
	if got := events[3]["panelAction"]; got != "click" {
		t.Errorf("channel_action[0] panelAction=%v want click", got)
	}
	if got := events[4]["panelAction"]; got != "success" {
		t.Errorf("channel_action[1] panelAction=%v want success", got)
	}
	if got := events[4]["isSuccessful"]; got != true {
		t.Errorf("channel_action[1] isSuccessful=%v want true", got)
	}
	if got := events[2]["source"]; got != "local_skip" {
		t.Errorf("security_result source=%v want local_skip（市场技能不过审）", got)
	}
}

// TestDesktopFingerprintVersionIsReal 断言桌面指纹版本对齐真机 5.11.3：
// skill_share 要求电脑端 ≥5.9.0，旧值（5.5.6）会被服务端按版本过滤掉。
func TestDesktopFingerprintVersionIsReal(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie"}
	fp := desktopFingerprint(a)
	if got := fp["ideVersion"]; got != "5.11.3" {
		t.Errorf("ideVersion=%v want 5.11.3", got)
	}
	if got := fp["extVersion"]; got != "5.11.3" {
		t.Errorf("extVersion=%v want 5.11.3", got)
	}
	if got := fp["extName"]; got != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", got)
	}
	if desktopUA != "WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0" {
		t.Errorf("desktopUA=%q want 5.11.3 + CLI 2.176.0", desktopUA)
	}
}
