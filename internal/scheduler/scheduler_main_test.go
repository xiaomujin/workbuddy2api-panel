package scheduler

import (
	"os"
	"testing"
)

// TestMain 把节奏类变量默认置零：本包多数用例只关心「有没有调用 / 调用几次」，
// 不该为真实节奏付秒级等待（签到后 1s、同槽错峰最长 225s、余额刷新 300ms）。
// 节奏本身由 stagger_test.go / retry_test.go 的专用用例显式设置并断言。
func TestMain(m *testing.M) {
	checkinBalanceGap = 0
	balanceAccountDelay = 0
	for i := range taskStagger {
		taskStagger[i].delay = 0
	}
	os.Exit(m.Run())
}
