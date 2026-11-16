package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// approxSec 断言 d 落在 want ± tol 内（释放时间的微小漂移来自 now 取值时刻）。
func approxSec(t *testing.T, label string, d, want, tol time.Duration) {
	t.Helper()
	if d < want-tol || d > want+tol {
		t.Errorf("%s: d=%v want %v±%v", label, d, want, tol)
	}
}

// TestEarliestServableInAccountCooldown 账号级软冷却 → 等待值 ≈ 冷却剩余；
// 池里另有健康号 → 归零（下游此刻重试就能成）。
func TestEarliestServableInAccountCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, 90*time.Second, "429 rate limit")

	d, ok := p.EarliestServableIn("glm-5.2", time.Now())
	if !ok {
		t.Fatal("ok=false：冷却账号会自愈，应给出等待值")
	}
	approxSec(t, "cooling", d, 90*time.Second, 2*time.Second)

	// 第二个健康号：最早等待归零（哪怕仍有人在冷却）。
	p.Add(&auth.Auth{UID: "u2"})
	d, ok = p.EarliestServableIn("glm-5.2", time.Now())
	if !ok || d != 0 {
		t.Fatalf("d=%v ok=%v want 0,true（有健康号即无需等待）", d, ok)
	}
}

// TestEarliestServableInSkipsDisabledAndPaused 禁用/暂停不随时间自愈，
// 不构成「稍后重试」的依据：全池皆此类 → ok=false（调用方据此不带 Retry-After）。
func TestEarliestServableInSkipsDisabledAndPaused(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "d"})
	p.Add(&auth.Auth{UID: "pz"})
	p.Disable("d", "banned by upstream")
	p.Pause("pz")

	if d, ok := p.EarliestServableIn("", time.Now()); ok {
		t.Fatalf("d=%v ok=true want ok=false（禁用/暂停不会自愈）", d)
	}
	// 只有冷却账号时 ok=true（对照：同样是"全不可用"，但可自愈）。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	if _, ok := p2.EarliestServableIn("", time.Now()); !ok {
		t.Fatal("冷却账号应 ok=true")
	}
}

// TestEarliestServableInModelLevel 6004 模型级冷却只挡被限流的模型，
// 等待值按 model 口径给出（账号对其他模型仍是 0 等待）。
func TestEarliestServableInModelLevel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetAt := time.Now().Add(2 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.3", "6004 model rate limit")

	d, ok := p.EarliestServableIn("glm-5.3", time.Now())
	if !ok {
		t.Fatal("ok=false：模型级冷却到期即恢复")
	}
	approxSec(t, "model cooled", d, 2*time.Minute, 2*time.Second)

	if d, ok := p.EarliestServableIn("glm-5.2", time.Now()); !ok || d != 0 {
		t.Fatalf("d=%v ok=%v want 0,true（其他模型不受该冷却影响）", d, ok)
	}
}
