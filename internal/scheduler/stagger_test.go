package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// pacingRecorder 记录各端点「首次被调用」的时刻（相对构造时刻），供错峰/间隔断言。
// 只看首次：错峰要证明的是「同槽第二族没有在第一族之后立刻发枪」，不是调用总数。
type pacingRecorder struct {
	mu    sync.Mutex
	start time.Time
	first map[string]time.Time
}

func newPacingRecorder() *pacingRecorder {
	return &pacingRecorder{start: time.Now(), first: map[string]time.Time{}}
}

func (r *pacingRecorder) mark(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.first[key]; !ok {
		r.first[key] = time.Now()
	}
}

// at 返回某端点首次被调用距构造时刻的偏移；未调用过 ok=false。
func (r *pacingRecorder) at(key string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.first[key]
	if !ok {
		return 0, false
	}
	return t.Sub(r.start), true
}

// server 同时模拟 billing（签到/余额）与 growth（旅行/连登）端点，其余路径一律
// 回成功信封（避免收尾链路把测试拖进错误分支）。
func (r *pacingRecorder) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/daily-checkin"):
			r.mark("checkin")
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(req.URL.Path, "/get-user-resource"):
			r.mark("balance")
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500}]}}}}`))
		case strings.HasSuffix(req.URL.Path, "/activity/growth/buddy/info"):
			r.mark("travel")
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":null}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
}

// setStagger 临时改某任务族的错峰偏移（0 = 立即），cleanup 复原。
func setStagger(t *testing.T, kind taskKind, d time.Duration) {
	t.Helper()
	for i := range taskStagger {
		if taskStagger[i].kind == kind {
			old := taskStagger[i].delay
			taskStagger[i].delay = d
			t.Cleanup(func() { taskStagger[i].delay = old })
			return
		}
	}
	t.Fatalf("taskStagger 未登记 kind=%v", kind)
}

// fastCheckin 临时改签到后间隔。
func fastCheckin(t *testing.T, d time.Duration) {
	t.Helper()
	old := checkinBalanceGap
	checkinBalanceGap = d
	t.Cleanup(func() { checkinBalanceGap = old })
}

// TestRunBatchStaggersTaskFamilies 同槽多族按 taskStagger 依次起跑：签到立即开跑，
// 旅行等足偏移才发第一枪。治的是「同一个号 3 秒内被签到 + 查余额 + 旅行连打」
// ——任务侧 429 的主要形态（checkin_hours 与 travel_hours 默认都是 [9,21]，同槽是常态）。
func TestRunBatchStaggersTaskFamilies(t *testing.T) {
	rec := newPacingRecorder()
	srv := rec.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")

	setStagger(t, taskCheckin, 0)
	setStagger(t, taskTravel, 120*time.Millisecond)

	s.runBatch(context.Background(), []taskKind{taskCheckin, taskTravel})

	cAt, okC := rec.at("checkin")
	tAt, okT := rec.at("travel")
	if !okC || !okT {
		t.Fatalf("两族都应跑过: checkin=%v travel=%v", okC, okT)
	}
	if tAt < 100*time.Millisecond {
		t.Errorf("travel 首调用偏移 %v，want >= 100ms（错峰未生效）", tAt)
	}
	if cAt >= 100*time.Millisecond {
		t.Errorf("checkin 首调用偏移 %v，want < 100ms（偏移 0 的族应立即开跑）", cAt)
	}
}

// TestRunBatchStaggerCanceledSkipsFamily ctx 在错峰等待中取消：被推迟的族直接放弃
// （下槽重来），已开跑的族不受影响。
func TestRunBatchStaggerCanceledSkipsFamily(t *testing.T) {
	rec := newPacingRecorder()
	srv := rec.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")

	setStagger(t, taskCheckin, 0)
	setStagger(t, taskTravel, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.runBatch(ctx, []taskKind{taskCheckin, taskTravel})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("runBatch 等满了错峰 %v，应在 ctx 取消后立即返回", elapsed)
	}

	if _, ok := rec.at("checkin"); !ok {
		t.Error("签到族应立即执行")
	}
	if d, ok := rec.at("travel"); ok {
		t.Errorf("ctx 取消后不应再跑被错峰推迟的旅行族（偏移偏移 %v）", d)
	}
}

// TestRunCheckinGapBetweenCheckinAndBalance 签到成功后到余额查询之间留 checkinBalanceGap：
// 同一个号刚写完 daily-checkin 紧接着查余额属于「同账号连打」，是被 429 的那一步。
func TestRunCheckinGapBetweenCheckinAndBalance(t *testing.T) {
	fastCheckin(t, 80*time.Millisecond)
	rec := newPacingRecorder()
	srv := rec.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")

	s.RunCheckinNow()

	cAt, okC := rec.at("checkin")
	bAt, okB := rec.at("balance")
	if !okC || !okB {
		t.Fatalf("签到与余额都应发生: checkin=%v balance=%v", okC, okB)
	}
	if d := bAt - cAt; d < 70*time.Millisecond {
		t.Errorf("签到→余额 间隔 %v，want >= 70ms（间隔未生效）", d)
	}
}
