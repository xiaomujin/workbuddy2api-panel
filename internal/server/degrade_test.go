package server

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries 边界：CST 视角下当日 00:00 之后 → 次日 00:00，
// 23:59 → 次日 00:00，正午 → 次日 00:00。用固定 +08:00 偏移计算，不依赖系统时区。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		// 期望 until 的 CST 时分秒恒为 00:00:00，且 strictly after now。
	}{
		{"23:59 -> next 00:00", time.Date(2026, 9, 11, 23, 59, 0, 0, cst)},
		{"00:00:01 -> next 00:00", time.Date(2026, 9, 11, 0, 0, 1, 0, cst)},
		{"12:00 -> next 00:00", time.Date(2026, 9, 11, 12, 0, 0, 0, cst)},
		{"00:00:00 exact -> next 00:00", time.Date(2026, 9, 11, 0, 0, 0, 0, cst)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextMidnightCST(tc.now)
			if !got.After(tc.now) {
				t.Fatalf("until %v not after now %v", got, tc.now)
			}
			// CST 视角下必须落在 00:00:00。
			if g := got.In(cst); g.Hour() != 0 || g.Minute() != 0 || g.Second() != 0 {
				t.Errorf("until CST = %02d:%02d:%02d, want 00:00:00", g.Hour(), g.Minute(), g.Second())
			}
		})
	}
}

// TestNextMidnightCSTCrossMonth 跨月：月末 23:59 → 次月 1 日 00:00。
func TestNextMidnightCSTCrossMonth(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, cst)
	got := nextMidnightCST(now)
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, cst)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestDegradeGateKeyedIsolation 降级作用域隔离：一段降级不影响同 realm 的其它段，
// 更不影响别的 realm。这是本次重构的核心语义（旧实现是进程级单例）。
func TestDegradeGateKeyedIsolation(t *testing.T) {
	var g degradeGate
	g.configure(30*time.Minute, 30*time.Minute, 3)
	if g.Active("cn", "segA") {
		t.Fatal("fresh gate should not be active")
	}
	g.Trigger("cn", "segA")
	if !g.Active("cn", "segA") {
		t.Fatal("触发的段应处于降级期")
	}
	if g.Active("cn", "segB") {
		t.Fatal("同 realm 的其它段不该被降级（这正是要修掉的污染）")
	}
	if g.Active("global", "segA") {
		t.Fatal("别的 realm 不该被降级")
	}
}

// TestDegradeGateTriggerNoRenewal 降级期内再次 Trigger 不续期。
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	var g degradeGate
	g.configure(30*time.Minute, 30*time.Minute, 3)
	g.Trigger("cn", "segA")
	first := g.entries[degradeKey{"cn", "segA"}]
	g.Trigger("cn", "segA")
	if second := g.entries[degradeKey{"cn", "segA"}]; !first.Equal(second) {
		t.Errorf("Trigger during active should not renew: first=%v second=%v", first, second)
	}
}

// TestDegradeGateTTLExpiry 短 TTL 到期后自动恢复（不再锁到次日 00:00）。
func TestDegradeGateTTLExpiry(t *testing.T) {
	var g degradeGate
	g.configure(20*time.Millisecond, 30*time.Minute, 3)
	g.Trigger("cn", "segA")
	if !g.Active("cn", "segA") {
		t.Fatal("刚触发应生效")
	}
	time.Sleep(40 * time.Millisecond)
	if g.Active("cn", "segA") {
		t.Fatal("TTL 到期后应自动恢复")
	}
	if active, until := g.Snapshot(); active || !until.IsZero() {
		t.Fatalf("过期项应被清扫，snapshot=(%v,%v)", active, until)
	}
}

// TestDegradeGateDayCompat ttl=0 时回退"到次日 00:00 CST"的老行为。
func TestDegradeGateDayCompat(t *testing.T) {
	var g degradeGate
	g.configure(0, 0, 0)
	g.Trigger("cn", "segA")
	until := g.entries[degradeKey{"cn", "segA"}]
	cst := time.FixedZone("CST", 8*60*60)
	if u := until.In(cst); u.Hour() != 0 || u.Minute() != 0 {
		t.Fatalf("day 兼容模式下 until=%v，应落在 CST 00:00", u)
	}
}

// TestDegradeGateGlobalFallback 同一 realm 内多个不同段全阶梯失败 → 该 realm 全局兜底。
func TestDegradeGateGlobalFallback(t *testing.T) {
	var g degradeGate
	g.configure(30*time.Minute, 30*time.Minute, 3)
	if g.NoteFailure("cn", "segA") {
		t.Fatal("1 段不该触发兜底")
	}
	if g.NoteFailure("cn", "segB") {
		t.Fatal("2 段不该触发兜底（阈值 3）")
	}
	if !g.NoteFailure("cn", "segC") {
		t.Fatal("第 3 个不同段应触发 realm 兜底")
	}
	if !g.Active("cn", "anyOtherSeg") {
		t.Fatal("兜底后该 realm 的任意段都应视为降级")
	}
	if g.Active("global", "anyOtherSeg") {
		t.Fatal("兜底不该跨 realm")
	}
}

// TestDegradeGateSameFailureNotCountedTwice 同一个段反复失败只算一段。
func TestDegradeGateSameFailureNotCountedTwice(t *testing.T) {
	var g degradeGate
	g.configure(30*time.Minute, 30*time.Minute, 2)
	g.NoteFailure("cn", "segA")
	if g.NoteFailure("cn", "segA") {
		t.Fatal("同一段重复失败不该凑数触发兜底")
	}
}

// TestDegradeGateClear 一键清：按段清 / 按 realm 清 / 全清。
func TestDegradeGateClear(t *testing.T) {
	var g degradeGate
	g.configure(30*time.Minute, 30*time.Minute, 3)
	g.Trigger("cn", "segA")
	g.Trigger("cn", "segB")
	g.Trigger("global", "segC")

	if n := g.Clear("cn", "segA"); n != 1 {
		t.Fatalf("按段清 n=%d want 1", n)
	}
	if g.Active("cn", "segA") {
		t.Fatal("segA 应已清除")
	}
	if !g.Active("cn", "segB") {
		t.Fatal("segB 不该被连带清除")
	}
	if n := g.Clear("cn", ""); n != 1 {
		t.Fatalf("按 realm 清 n=%d want 1", n)
	}
	if g.ActiveAny("") == false {
		t.Fatal("global 域仍在降级，ActiveAny 应为 true")
	}
	if n := g.Clear("", ""); n != 1 {
		t.Fatalf("全清 n=%d want 1", n)
	}
	if active, _ := g.Snapshot(); active {
		t.Fatal("全清后不该还有降级")
	}
}

// TestDegradeGateSnapshotAndScopes 快照聚合与作用域列表（面板/status 的唯一出口）。
func TestDegradeGateSnapshotAndScopes(t *testing.T) {
	var g degradeGate
	if active, until := g.Snapshot(); active || !until.IsZero() {
		t.Fatalf("fresh gate snapshot = (%v, %v), want (false, zero)", active, until)
	}
	g.configure(30*time.Minute, 30*time.Minute, 3)
	g.Trigger("cn", "segA")
	active, until := g.Snapshot()
	if !active || until.IsZero() {
		t.Fatalf("Trigger 后 snapshot=(%v,%v)", active, until)
	}
	scopes := g.Scopes()
	if len(scopes) != 1 {
		t.Fatalf("scopes=%d want 1", len(scopes))
	}
	if scopes[0].Realm != "cn" || scopes[0].Seg != "segA" {
		t.Fatalf("scope=%+v", scopes[0])
	}
}
