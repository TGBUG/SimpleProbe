package collect

import (
	"errors"
	"testing"
	"time"
)

// TestCollector_CounterRegressionFailsRound 覆盖采集器里 CPUPct 报错的分支。
//
// 计数器倒退（回绕、或是被人为写坏）时整轮必须失败，而不是算出一个
// 荒谬的使用率再上报上去。
func TestCollector_CounterRegressionFailsRound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, files := newTestCollector(t, &now)

	if _, err := c.Sample(); !errors.Is(err, ErrWarmup) {
		t.Fatalf("首次采样应返回 ErrWarmup，得到 %v", err)
	}

	// 总时间从 1000 掉回 200：明显的倒退。
	now = now.Add(30 * time.Second)
	files["/proc/stat"] = statLine(10, 0, 10, 180, 0, 0, 0, 0)

	_, err := c.Sample()
	if err == nil {
		t.Fatal("计数器倒退时应当报错")
	}
	if !errors.Is(err, errCounterWentBackwards) {
		t.Errorf("错误 = %v，期望 errCounterWentBackwards", err)
	}
}
