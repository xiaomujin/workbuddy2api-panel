package scheduler

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// captureLog 把标准 logger 输出接到内存缓冲，返回读取函数（cleanup 自动复位）。
// 日志断言只看「出现了什么行」，不依赖输出顺序之外的时序。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(old) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// countLines 统计 buf 中含 sub 的行数。
func countLines(buf, sub string) int {
	n := 0
	for _, line := range strings.Split(buf, "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// 余额刷新降噪：连续失败只在第 1 轮打明细，之后各轮只出汇总行。
func TestBalanceRefreshSuppressesRepeatFailureLogs(t *testing.T) {
	logs := captureLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error_msg":"Too Many Requests"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"u1", "u2"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// 第 1 轮：两个号都是「新故障」→ 两条明细节 + 一行汇总。
	s.RunBalanceRefreshNow()
	round1 := logs()
	if n := countLines(round1, "upstream soft_rate"); n != 2 {
		t.Errorf("第 1 轮明细行 %d 条 want 2: %s", n, round1)
	}
	if n := countLines(round1, "balance: 本轮 2/2 账号查询失败"); n != 1 {
		t.Errorf("第 1 轮应有汇总行: %s", round1)
	}
	if strings.Contains(round1, "只计数不重复打印") {
		t.Errorf("首轮没有连续失败，不该出现降噪文案: %s", round1)
	}
	// 第 2 轮：同两个号仍失败 → 不得再打明细节，只出一行汇总（含降噪计数）。
	before := len(round1)
	s.RunBalanceRefreshNow()
	round2 := logs()[before:]
	if n := countLines(round2, "upstream soft_rate"); n != 0 {
		t.Errorf("第 2 轮仍打明细 %d 条，want 0: %s", n, round2)
	}
	if n := countLines(round2, "本轮 2/2 账号查询失败"); n != 1 {
		t.Errorf("第 2 轮应有汇总行: %s", round2)
	}
	if !strings.Contains(round2, "其中 2 个连续失败只计数不重复打印") {
		t.Errorf("第 2 轮汇总应说明降噪数量: %s", round2)
	}
}

// 余额刷新恢复：失败后成功，打一行「恢复」并清计数。
func TestBalanceRefreshLogsRecoveryAfterFailure(t *testing.T) {
	logs := captureLog(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error_msg":"Too Many Requests"}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunBalanceRefreshNow() // 失败
	s.RunBalanceRefreshNow() // 恢复
	buf := logs()
	if n := countLines(buf, "恢复（连续失败 1 轮后）"); n != 1 {
		t.Errorf("应有一行恢复日志: %s", buf)
	}
	if st, _ := p.Status("u1"); st.Credits != 500 {
		t.Errorf("恢复轮应写入 credits，got %d", st.Credits)
	}
	if n := len(s.balanceFailStreak); n != 0 {
		t.Errorf("恢复后连续失败计数应清零，got %v", s.balanceFailStreak)
	}
}

// 签到汇总行：一轮一行，把「成功/已签到/未签到 + 429 已排补跑」讲清楚。
func TestCheckinRoundSummaryLine(t *testing.T) {
	logs := captureLog(t)
	stub := &retryStub{checkin429First: 99} // 签到恒 429
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, _ := newRetryScheduler(t, srv)

	s.RunCheckinNow()
	buf := logs()
	if !strings.Contains(buf, "checkin 汇总: 签到 1 号（成功 0 · 已签到 0 · 未签到 1，其中 429 1 已排补跑）") {
		t.Errorf("汇总行不符: %s", buf)
	}
}

// 签到成功 + 余额失败：汇总行要同时交代两件事（余额 429 也要标出来）。
func TestCheckinRoundSummaryIncludesBalanceFailure(t *testing.T) {
	logs := captureLog(t)
	stub := &retryStub{balance429First: 99} // 签到成功、余额恒 429
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, _ := newRetryScheduler(t, srv)

	s.RunCheckinNow()
	buf := logs()
	if !strings.Contains(buf, "签到 1 号（成功 1 · 已签到 0 · 未签到 0）") {
		t.Errorf("汇总行应含签到成功 1: %s", buf)
	}
	if !strings.Contains(buf, "余额查询失败 1（429 1 已排补跑）") {
		t.Errorf("汇总行应含余额失败与 429 标注: %s", buf)
	}
}
