package main

import (
	"testing"
	"time"
)

// 本文件钉住 repair 段（11-128 修复阶梯）的配置契约：
//   - 缺省全开（误报处理必须默认生效，否则合法流量被逐字指纹误杀）；
//   - degrade_ttl 支持 "day"/"0"（老行为：到次日 00:00）与普通时长；
//   - 非法值 fail fast（拼错一个时长不该静默回落到默认）。

func TestRepairDefaults(t *testing.T) {
	c := Default()
	if !c.Repair.Enabled {
		t.Error("repair.enabled 缺省必须为 true")
	}
	if !c.Repair.EnsureSystemFirst || !c.Repair.SplitSystem || !c.Repair.MoveSystemToUser {
		t.Error("L0/L2/L3 缺省必须全开")
	}
	if c.Repair.Budget != 3 {
		t.Errorf("budget 缺省 = %d want 3", c.Repair.Budget)
	}
	if !c.Repair.Memory.Enabled {
		t.Error("段指纹记忆缺省必须开启")
	}
	if c.Repair.Memory.PreapplyAfter != 2 {
		t.Errorf("preapply_after 缺省 = %d want 2", c.Repair.Memory.PreapplyAfter)
	}
}

func TestRepairNormalizeDurations(t *testing.T) {
	cases := []struct {
		name    string
		ttl     string
		wantDur time.Duration
		wantRaw string
	}{
		{"缺省 30m", "", 30 * time.Minute, "30m"},
		{"显式 10m", "10m", 10 * time.Minute, "10m"},
		{"下钳到 5m", "30s", 5 * time.Minute, "30s"},
		{"上钳到 24h", "72h", 24 * time.Hour, "72h"},
		{"day 哨兵（负值）", "day", -1, "day"},
		{"0 = day", "0", -1, "day"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Repair.DegradeTTL = tc.ttl
			if err := c.normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if c.RepairDegradeTTLDur != tc.wantDur {
				t.Fatalf("dur=%v want %v", c.RepairDegradeTTLDur, tc.wantDur)
			}
			if c.Repair.DegradeTTL != tc.wantRaw {
				t.Fatalf("raw=%q want %q", c.Repair.DegradeTTL, tc.wantRaw)
			}
		})
	}
}

func TestRepairNormalizeFallbackTTL(t *testing.T) {
	c := Default()
	c.Repair.GlobalFallbackTTL = "0"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.RepairFallbackTTLDur != 0 {
		t.Fatalf("fallback ttl=0 应关闭兜底，got %v", c.RepairFallbackTTLDur)
	}
	// 非法时长必须报错（不静默回落）。
	c2 := Default()
	c2.Repair.DegradeTTL = "半小时"
	if err := c2.normalize(); err == nil {
		t.Fatal("非法 degrade_ttl 应报错")
	}
}

func TestRepairJSONOverride(t *testing.T) {
	c := Default()
	raw := []byte(`{"repair":{"enabled":false,"budget":1,"degrade_ttl":"5m","memory":{"enabled":false}}}`)
	if _, err := ParseConfigInto(raw, c); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Repair.Enabled {
		t.Error("显式 false 应关闭阶梯")
	}
	if c.Repair.Budget != 1 {
		t.Errorf("budget=%d want 1", c.Repair.Budget)
	}
	if c.Repair.Memory.Enabled {
		t.Error("显式 false 应关闭记忆")
	}
	// 未出现在 JSON 里的键保留默认（split/move 仍开着）。
	if !c.Repair.SplitSystem || !c.Repair.MoveSystemToUser {
		t.Error("未覆盖的开关应保留缺省 true")
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.RepairDegradeTTLDur != 5*time.Minute {
		t.Errorf("degrade ttl=%v want 5m", c.RepairDegradeTTLDur)
	}
}
