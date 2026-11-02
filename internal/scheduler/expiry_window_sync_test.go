package scheduler

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestExpiryBucketWindowsInSync 守住「上游分档窗口」与「池内策略窗口」不漂移。
//
// 上游按固定 24h/72h 统计 Expiring24h/72h（它不该知道池内策略），池内用
// ExpiryPolicy.FastWindow/MidWindow 判定档位。两者一旦不一致，档位判定与金额
// 统计就会错位（例如策略改 12h、上游仍按 24h 统计 → 快档金额被高估）。
func TestExpiryBucketWindowsInSync(t *testing.T) {
	pol := pool.DefaultExpiryPolicy()
	if pol.FastWindow != upstream.ExpiryBucketFast {
		t.Fatalf("FastWindow=%v want %v (upstream.ExpiryBucketFast)", pol.FastWindow, upstream.ExpiryBucketFast)
	}
	if pol.MidWindow != upstream.ExpiryBucketMid {
		t.Fatalf("MidWindow=%v want %v (upstream.ExpiryBucketMid)", pol.MidWindow, upstream.ExpiryBucketMid)
	}
}
