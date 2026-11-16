package scheduler

import (
	"context"
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

// fastRetryBackoff 把退避序列缩到毫秒级（生产 60s/120s），cleanup 复原。
func fastRetryBackoff(t *testing.T) {
	t.Helper()
	old := retryBackoff
	retryBackoff = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond}
	t.Cleanup(func() { retryBackoff = old })
}

// softRateErr 构造 429 分类信封（与 upstream.Classify 的产物同形）。
func softRateErr() error {
	return &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "Too Many Requests"}
}

// retryStub 计费端点桩：可指定「前 N 次返回 429」。
type retryStub struct {
	checkinCalls, balanceCalls atomic.Int32
	checkin429First            int32
	balance429First            int32
}

func (r *retryStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/daily-checkin"):
			n := r.checkinCalls.Add(1)
			if n <= atomic.LoadInt32(&r.checkin429First) {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error_msg":"Too Many Requests"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(req.URL.Path, "/get-user-resource"):
			n := r.balanceCalls.Add(1)
			if n <= atomic.LoadInt32(&r.balance429First) {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error_msg":"Too Many Requests"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":777}]}}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// newRetryScheduler 构造只含一个 CN 个人号的调度器。
func newRetryScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestRunCheckinQueuesRetryOn429 签到 429 → 入队 → 到期补跑成功并记 NoteCheckinDone。
// 这是「09:00 签到被限流」不再等于当天放弃的最小闭环。
func TestRunCheckinQueuesRetryOn429(t *testing.T) {
	fastRetryBackoff(t)
	stub := &retryStub{checkin429First: 1}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newRetryScheduler(t, srv)

	s.RunCheckinNow()
	if st, _ := p.Status("u1"); st.CheckinDone {
		t.Fatal("429 不应记为已签到")
	}
	if n := len(s.retries); n != 1 {
		t.Fatalf("队列长度=%d want 1（429 应入队补跑）", n)
	}

	time.Sleep(25 * time.Millisecond) // 越过 retryBackoff[0]=20ms
	s.runDueRetries(context.Background())

	if n := stub.checkinCalls.Load(); n != 2 {
		t.Fatalf("daily-checkin 调用 %d 次 want 2（原调用 + 1 次补跑）", n)
	}
	if st, _ := p.Status("u1"); !st.CheckinDone {
		t.Error("补跑成功后应记 CheckinDone")
	}
	if n := len(s.retries); n != 0 {
		t.Errorf("成功后队列应清空，got %d", n)
	}
}

// TestRunCheckinQueuesRetryForBalance429 签到成功但余额查询 429 → 入队 user-resource 补跑，
// 补跑成功后 credits 落地（治「签到成功、余额一直显示旧值」）。
func TestRunCheckinQueuesRetryForBalance429(t *testing.T) {
	fastRetryBackoff(t)
	stub := &retryStub{balance429First: 1}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := newRetryScheduler(t, srv)

	s.RunCheckinNow()
	if st, _ := p.Status("u1"); !st.CheckinDone {
		t.Fatal("签到应成功")
	}
	if st, _ := p.Status("u1"); st.Credits != 0 {
		t.Fatalf("429 那次不应写入 credits，got %d", st.Credits)
	}
	if n := len(s.retries); n != 1 {
		t.Fatalf("队列长度=%d want 1（余额 429 应入队）", n)
	}
	if op := s.retries[0].op; op != retryUserResource {
		t.Fatalf("队列项 op=%v want %v", op, retryUserResource)
	}

	time.Sleep(25 * time.Millisecond)
	s.runDueRetries(context.Background())

	if st, _ := p.Status("u1"); st.Credits != 777 {
		t.Fatalf("补跑后 credits=%d want 777", st.Credits)
	}
}

// TestRetryGivesUpAfterMaxAttempts 持续 429：补跑 maxRetryAttempts 次后放弃，
// 队列清空（不再对上游空转——剩下的交给下一轮小时排程）。
func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	fastRetryBackoff(t)
	stub := &retryStub{checkin429First: 99}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, _ := newRetryScheduler(t, srv)

	s.RunCheckinNow()
	for i := 0; i < maxRetryAttempts+2; i++ {
		time.Sleep(60 * time.Millisecond) // 越过两级退避
		s.runDueRetries(context.Background())
	}

	if n := stub.checkinCalls.Load(); n != 1+maxRetryAttempts {
		t.Errorf("daily-checkin 调用 %d 次 want %d（原调用 + 上限次补跑）", n, 1+maxRetryAttempts)
	}
	if n := len(s.retries); n != 0 {
		t.Errorf("放弃后队列应清空，got %d", n)
	}
}

// TestNoteRetryableErrOnlyForSoftRate 只有 429 入队：5xx / 业务错误都不排补跑
//（5xx 已由 upstream 的有界重试覆盖，业务错误重放无意义）。
func TestNoteRetryableErrOnlyForSoftRate(t *testing.T) {
	s, _ := newRetryScheduler(t, httptest.NewServer(http.NotFoundHandler()))

	s.noteRetryableErr("u1", retryCheckin, 0, &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"})
	s.noteRetryableErr("u1", retryCheckin, 0, &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: "bad"})
	s.noteRetryableErr("u1", retryUserResource, 0, nil)
	if n := len(s.retries); n != 0 {
		t.Fatalf("非 429 不应入队，got %d", n)
	}

	s.noteRetryableErr("u1", retryCheckin, 0, softRateErr())
	if n := len(s.retries); n != 1 {
		t.Fatalf("429 应入队，got %d", n)
	}
}

// TestRetryDelayFor 延迟口径：上游 Retry-After 优先；否则按退避序列；统一钳到上限。
func TestRetryDelayFor(t *testing.T) {
	withRetryAfter := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, RetryAfter: 90 * time.Second}
	if d := retryDelayFor(withRetryAfter, 0); d != 90*time.Second {
		t.Errorf("带 Retry-After: %v want 90s（上游口径优先）", d)
	}
	if d := retryDelayFor(softRateErr(), 0); d != retryBackoff[0] {
		t.Errorf("无 Retry-After trials=0: %v want %v", d, retryBackoff[0])
	}
	if d := retryDelayFor(softRateErr(), 1); d != retryBackoff[1] {
		t.Errorf("无 Retry-After trials=1: %v want %v", d, retryBackoff[1])
	}
	if d := retryDelayFor(softRateErr(), 9); d != retryBackoff[len(retryBackoff)-1] {
		t.Errorf("越界 tries 应回落末档: %v", d)
	}
	huge := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, RetryAfter: 6 * time.Hour}
	if d := retryDelayFor(huge, 0); d != retryDelayMax {
		t.Errorf("超上限应钳到 %v，got %v", retryDelayMax, d)
	}
}

// TestScheduleRetryDedupesSameOp 同 (uid, op) 只留一条：面板手点与排程同槽撞车
// 不该在队列里排两份。
func TestScheduleRetryDedupesSameOp(t *testing.T) {
	s, _ := newRetryScheduler(t, httptest.NewServer(http.NotFoundHandler()))
	s.scheduleRetry("u1", retryCheckin, 0, time.Minute)
	s.scheduleRetry("u1", retryCheckin, 0, time.Minute)
	s.scheduleRetry("u1", retryUserResource, 0, time.Minute)
	if n := len(s.retries); n != 2 {
		t.Fatalf("队列长度=%d want 2（同 op 去重，异 op 各一条）", n)
	}
}

// TestStartRetryWorkerRunsDue 后台循环按 dueAt 自动补跑，ctx 取消即停。
func TestStartRetryWorkerRunsDue(t *testing.T) {
	fastRetryBackoff(t)
	stub := &retryStub{checkin429First: 99}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, _ := newRetryScheduler(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartRetryWorker(ctx)

	s.noteRetryableErr("u1", retryCheckin, 0, softRateErr())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stub.checkinCalls.Load() >= 1 { // 补跑已发生
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("补跑未在 2s 内发生（daily-checkin 调用 %d 次）", stub.checkinCalls.Load())
}
