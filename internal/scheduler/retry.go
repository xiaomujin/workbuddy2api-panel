// retry.go 任务侧 429 的「稍后重试」队列。
//
// 为什么需要：计费类调用的重试器（upstream.retryBillingTransient）只补打 5xx 与网络
// 抖动（2s/4s）——限流原样重试只会再失败一次，所以它把 429 排除在外是对的。此前的
// 兜底只有「下一个小时槽」：签到 09→21 差 12 小时（等于当天放弃），余额查询靠 5 分钟
// 周期（够用，但 429 仍会周期性地打在日志里）。本队列补上中间档：按上游明示的
// Retry-After（没有则 60s/120s 退避）排一次性延迟补跑，最多 maxRetryAttempts 次，
// 仍失败就交给下一轮小时排程。
//
// 只收**幂等**调用：
//   - retryCheckin：daily-checkin 幂等（重复签到 = 上游「今天已签到」，按成功记）；
//   - retryUserResource：get-user-resource 只读。
//
// travel 刻意不入队：depart 消耗当日次数、claim 是一次性领奖，重放有真实代价——交给
// 21 点那轮状态机自然推进（状态机按 travel/status 分派，等价于一次幂等重放）。
//
// 队列只存内存：进程重启即丢。代价是「这一次补跑没了」，与丢一个排程窗口等价，不值得
// 为它引入持久化（state.json 承担账号状态，任务重试是纯进程内的节奏）。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// retryOp 可补跑的任务操作。
type retryOp string

const (
	retryCheckin      retryOp = "checkin"
	retryUserResource retryOp = "user-resource"
)

// maxRetryAttempts 单账号单操作最多补跑次数（不含最初那次调用）。
const maxRetryAttempts = 2

// retryBackoff 上游未明示等待时长时的退避序列：第 1 次补跑前 60s、第 2 次前 120s。
// 比 pool 的 soft_rate（600s 起）短一个量级——任务侧 429 通常几十秒就过，罚 10 分钟
// 等于把当天签到推到 21 点。测试可改（retry_test.go 用毫秒级序列）。
var retryBackoff = []time.Duration{60 * time.Second, 120 * time.Second}

// retryDelayMax 单次补跑延迟上限：再长不如等下一个小时槽。
const retryDelayMax = 30 * time.Minute

// retrySweepInterval 巡检间隔上限：没有到期项时也定期醒一次（防墙钟跳变漏算）。
const retrySweepInterval = 5 * time.Minute

// retryItem 一条待补跑记录。
type retryItem struct {
	uid   string
	op    retryOp
	dueAt time.Time
	tries int // 已完成的补跑次数（0 = 首次补跑还没跑）
}

// noteRetryableErr 任务调用失败后的统一入口：**仅 429（ErrSoftRate）入队补跑**，
// 其余错误不动。5xx / 网络抖动已由 upstream 的有界重试（2s/4s）覆盖；业务错误
// （已签到、参数错）重放只会原样再失败一次。
func (s *Scheduler) noteRetryableErr(uid string, op retryOp, tries int, err error) {
	if !isSoftRateErr(err) {
		return
	}
	s.scheduleRetry(uid, op, tries, retryDelayFor(err, tries))
}

// scheduleRetry 把一次补跑排进队（tries = 已完成的补跑次数）。超出上限即放弃——
// 剩下的交给下一轮小时排程，不再对上游空转。
func (s *Scheduler) scheduleRetry(uid string, op retryOp, tries int, delay time.Duration) {
	if uid == "" {
		return
	}
	if tries >= maxRetryAttempts {
		log.Printf("retry %s %s: 已补跑 %d 次仍 429，放弃（等下一个排程时点）",
			logfmt.Label(uid, s.nickOf(uid)), op, tries)
		return
	}
	it := retryItem{uid: uid, op: op, dueAt: time.Now().Add(delay), tries: tries}

	s.retryMu.Lock()
	// 同 (uid, op) 只留一条：面板手点与排程同槽撞车时不该在队列里排两份。
	for i, old := range s.retries {
		if old.uid == it.uid && old.op == it.op {
			s.retries[i] = it
			s.retryMu.Unlock()
			s.pokeRetryWake()
			return
		}
	}
	s.retries = append(s.retries, it)
	s.retryMu.Unlock()
	s.pokeRetryWake()

	log.Printf("retry %s %s: 429 → %s 后补跑（第 %d 次）",
		logfmt.Label(uid, s.nickOf(uid)), op, delay.Round(time.Second), tries+1)
}

// nickOf 取账号昵称供日志标签（账号已移除时返回空串，Label 自动退回 uid8）。
func (s *Scheduler) nickOf(uid string) string {
	if st, ok := s.cfg.Pool.Status(uid); ok {
		return st.Nickname
	}
	return ""
}

// pokeRetryWake 通知补跑循环「队列变了，立即重算最早到期」。
func (s *Scheduler) pokeRetryWake() {
	select {
	case s.retryWake <- struct{}{}:
	default:
	}
}

// StartRetryWorker 启动补跑队列的后台循环，ctx 取消即停。
// 与 Run / StartBalanceRefresh 并列启动（见 cmd/server/main.go）。
func (s *Scheduler) StartRetryWorker(ctx context.Context) {
	go func() {
		for {
			wait := retrySweepInterval
			if due, ok := s.nextRetryDue(); ok {
				wait = time.Until(due)
				if wait < 0 {
					wait = 0
				}
				if wait > retrySweepInterval {
					wait = retrySweepInterval
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.retryWake:
				timer.Stop() // 新入队：按最近的 dueAt 重算，不睡满
			case <-timer.C:
			}
			s.runDueRetries(ctx)
		}
	}()
}

// nextRetryDue 返回队列中最早的到期时刻（空队列 ok=false）。
func (s *Scheduler) nextRetryDue() (time.Time, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	var best time.Time
	for _, it := range s.retries {
		if best.IsZero() || it.dueAt.Before(best) {
			best = it.dueAt
		}
	}
	return best, !best.IsZero()
}

// runDueRetries 取出所有到期项并串行补跑：量级 = 账号数 × 2，串行足够，也天然限速。
func (s *Scheduler) runDueRetries(ctx context.Context) {
	now := time.Now()
	s.retryMu.Lock()
	var due, rest []retryItem
	for _, it := range s.retries {
		if !it.dueAt.After(now) {
			due = append(due, it)
			continue
		}
		rest = append(rest, it)
	}
	s.retries = rest
	s.retryMu.Unlock()

	for _, it := range due {
		if ctx.Err() != nil {
			return // 停机：未跑的到期项直接丢（内存队列语义，重启后由排程重建）
		}
		s.runRetryOne(it)
	}
}

// runRetryOne 执行一条补跑项；仍 429 时按 tries+1 再排（直到 maxRetryAttempts）。
func (s *Scheduler) runRetryOne(it retryItem) {
	a := s.cfg.Pool.AuthByUID(it.uid)
	if a == nil || a.RefreshTokenValue() == "" {
		return // 账号已移除 / 无 refresh：静默丢弃（发不出任何调用）
	}
	// 排队期间账号被禁用：与排程口径一致（include_disabled_in_tasks 缺省跳过禁用号），
	// 别让补跑变成绕过该开关的后门。
	if st, ok := s.cfg.Pool.Status(it.uid); ok && st.Disabled && !s.includeDisabledInTasks() {
		return
	}
	label := logfmt.Label(it.uid, a.Nickname)

	switch it.op {
	case retryCheckin:
		// 域门控与 RunCheckinNow 一致：global 无签到体系、企业版上游直接拒。
		if a.IsGlobal() || a.IsEnterprise() {
			return
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// 「今天已签到」是幂等成功：这通常意味着原调用其实已落库（429 是它的
			// 响应噪声），按成功记，不再补跑。
			if upstream.IsAlreadyCheckin(err) {
				s.cfg.Pool.NoteCheckinDone(it.uid)
				log.Printf("checkin %s: 今天已签到（幂等·补跑）", label)
				return
			}
			log.Printf("checkin %s: 补跑失败（第 %d 次）: %v", label, it.tries+1, err)
			s.noteRetryableErr(it.uid, retryCheckin, it.tries+1, err)
			return
		}
		s.cfg.Pool.NoteCheckinDone(it.uid)
		log.Printf("checkin %s: 签到成功（补跑·第 %d 次）", label, it.tries+1)
		// 签到成功后余额才是新的：顺带刷新一次（与 RunCheckinNow 同口径）。
		if err := s.refreshBalanceOne(a); err != nil {
			log.Printf("user-resource %s: %v", label, err)
			s.noteRetryableErr(it.uid, retryUserResource, 0, err)
		}
	case retryUserResource:
		if err := s.refreshBalanceOne(a); err != nil {
			log.Printf("user-resource %s: 补跑失败（第 %d 次）: %v", label, it.tries+1, err)
			s.noteRetryableErr(it.uid, retryUserResource, it.tries+1, err)
			return
		}
		log.Printf("user-resource %s: 余额刷新成功（补跑·第 %d 次）", label, it.tries+1)
	}
}

// isSoftRateErr 报告 err 是否为 429 软限流（任务侧唯一值得「稍后重试」的错误形态）。
func isSoftRateErr(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Kind == upstream.ErrSoftRate
}

// retryDelayFor 计算补跑延迟：上游明示的 Retry-After 优先（那是它自己的口径），
// 否则按 retryBackoff[tries]；两者都钳到 retryDelayMax。
func retryDelayFor(err error, tries int) time.Duration {
	d := retryBackoff[len(retryBackoff)-1]
	if tries >= 0 && tries < len(retryBackoff) {
		d = retryBackoff[tries]
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.RetryAfter > 0 {
		d = ue.RetryAfter
	}
	if d > retryDelayMax {
		d = retryDelayMax
	}
	if d <= 0 {
		d = retryBackoff[0]
	}
	return d
}
