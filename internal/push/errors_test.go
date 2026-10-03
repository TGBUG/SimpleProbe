package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRejectedError_Error 锁住错误文案。
// agent 侧会把这条信息直接打给人看，所以它必须同时说清“哪一类拒绝”和“为什么”。
func TestRejectedError_Error(t *testing.T) {
	withCode := &RejectedError{Status: 400, Code: "invalid_field", Message: "cpu_pct 超出范围"}
	want := "server 拒绝上报（HTTP 400, invalid_field）：cpu_pct 超出范围"
	if got := withCode.Error(); got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}

	noCode := &RejectedError{Status: 401, Message: "token 不匹配"}
	want = "server 拒绝上报（HTTP 401）：token 不匹配"
	if got := noCode.Error(); got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
}

// TestSend_RealSleep 走真实的 time.Timer 路径（注入的 SleepFn 覆盖不到它），
// 同时验证退避确实发生了。
func TestSend_RealSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := New(Options{
		ServerURL: srv.URL, Node: "web01", Token: "t",
		Attempts: 2, BaseDelay: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}

	start := time.Now()
	if err := c.Send(context.Background(), sampleReport()); err == nil {
		t.Fatal("期望报错")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("应当等待了退避时间，实际只用了 %v", elapsed)
	}
}

// TestSend_RealSleepAbortsOnCancelledContext 覆盖真实睡眠里的 ctx 分支：
// 上下文已取消时，退避必须立刻中断而不是傻等。
func TestSend_RealSleepAbortsOnCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel() // 进入退避前就已取消

	c, err := New(Options{
		ServerURL: srv.URL, Node: "web01", Token: "t",
		Attempts: 5, BaseDelay: time.Hour, // 若没被中断，这条测试会挂住
	})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Send(ctx, sampleReport()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("期望报错")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("上下文取消后没有及时返回")
	}
}
