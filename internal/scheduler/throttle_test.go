package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// concurrencyStub 记录峰值并发：每个余额请求先 ++inFlight、睡一会儿、再 --。
type concurrencyStub struct {
	inFlight atomic.Int32
	peak     atomic.Int32
	hold     time.Duration
	calls    atomic.Int32
}

func (c *concurrencyStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/get-user-resource") {
			http.Error(w, "not found", 404)
			return
		}
		c.calls.Add(1)
		n := c.inFlight.Add(1)
		for {
			peak := c.peak.Load()
			if n <= peak || c.peak.CompareAndSwap(peak, n) {
				break
			}
		}
		time.Sleep(c.hold)
		c.inFlight.Add(-1)
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500}]}}}}`))
	})
}

// TestRunBalanceRefreshThrottlesRefresh 余额刷新按并发 2 + 派发间隔摊开：
// 治「N 个号同一秒打同一端点」——2026-11 现场 user-resource 全员 429 的形态。
func TestRunBalanceRefreshThrottlesRefresh(t *testing.T) {
	old := balanceAccountDelay
	balanceAccountDelay = 30 * time.Millisecond
	t.Cleanup(func() { balanceAccountDelay = old })

	stub := &concurrencyStub{hold: 50 * time.Millisecond}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	start := time.Now()
	s.RunBalanceRefreshNow()
	elapsed := time.Since(start)

	if n := stub.calls.Load(); n != 5 {
		t.Fatalf("余额查询 %d 次 want 5", n)
	}
	if peak := stub.peak.Load(); peak > balanceRefreshConcurrency {
		t.Errorf("峰值并发 %d 超过上限 %d", peak, balanceRefreshConcurrency)
	}
	// 5 个号 × 30ms 间隔 ≈ 120ms 发车耗时（末个 +50ms 在途）；少于 4 个间隔说明没限速。
	if minWant := 4 * balanceAccountDelay; elapsed < minWant {
		t.Errorf("总耗时 %v < %v（派发间隔未生效）", elapsed, minWant)
	}
}

// TestRunBalanceRefreshDelayZeroStillCoversAll delay=0 时不该跳过任何账号（TestMain 的
// 零节奏只在测试里生效，这里确认限速逻辑不会把账号漏掉）。
func TestRunBalanceRefreshDelayZeroStillCoversAll(t *testing.T) {
	stub := &concurrencyStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBalanceRefreshNow()

	if n := stub.calls.Load(); n != 3 {
		t.Fatalf("余额查询 %d 次 want 3", n)
	}
	for _, uid := range []string{"a", "b", "c"} {
		if st, _ := p.Status(uid); st.Credits != 500 {
			t.Errorf("%s credits=%d want 500", uid, st.Credits)
		}
	}
}
