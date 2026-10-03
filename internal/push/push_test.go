package push

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

func sampleReport() *protocol.Report {
	return &protocol.Report{
		V: 1, Node: "web01", IntervalS: 30,
		Load:    []float64{0.42, 0.55, 0.61},
		CPUPct:  12.3,
		Mem:     protocol.Mem{Used: 3221225472, Total: 8589934592},
		Disk:    []protocol.Disk{{Mount: "/", Used: 21474836480, Total: 53687091200}},
		UptimeS: 1234567, AgentVersion: "0.1.0",
	}
}

// noSleep 让重试测试瞬间跑完，同时记录每次退避时长。
func noSleep(delays *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}
}

func mustClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.SleepFn == nil {
		var discard []time.Duration
		opts.SleepFn = noSleep(&discard)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	return c
}

func TestSend_Success(t *testing.T) {
	var calls int32
	var gotPath, gotAuth, gotCT, gotUA string
	var gotRep protocol.Report

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")

		// 直接用 server 的真实入口校验 agent 发出的字节流——
		// 这条断言保证“agent 发得出去”与“server 收得下来”是同一套口径。
		rep, err := protocol.DecodeStrict(r.Body)
		if err != nil {
			t.Errorf("server 拒绝了 agent 发出的载荷: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotRep = *rep
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := mustClient(t, Options{ServerURL: srv.URL, Node: "web01", Token: "s3cret"})
	if err := c.Send(context.Background(), sampleReport()); err != nil {
		t.Fatalf("Send() 失败: %v", err)
	}

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("请求次数 = %d，期望 1", n)
	}
	if gotPath != "/api/v1/report" {
		t.Errorf("路径 = %q，期望 /api/v1/report", gotPath)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotUA != "probe-agent" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotRep.Node != "web01" || gotRep.IntervalS != 30 || gotRep.CPUPct != 12.3 {
		t.Errorf("服务端解出的载荷不对: %+v", gotRep)
	}
	if len(gotRep.Disk) != 1 || gotRep.Disk[0].Mount != "/" {
		t.Errorf("磁盘字段丢失: %+v", gotRep.Disk)
	}
}

func TestSend_TrimsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := mustClient(t, Options{ServerURL: srv.URL + "/", Node: "web01", Token: "t"})
	if err := c.Send(context.Background(), sampleReport()); err != nil {
		t.Fatalf("Send() 失败: %v", err)
	}
	if gotPath != "/api/v1/report" {
		t.Errorf("路径 = %q，期望 /api/v1/report", gotPath)
	}
}

// TestSend_RejectedDoesNotRetry 是规格 §4.7 里最要紧的一条行为：
// 4xx 表示“我发的东西不对”，重试一万次结果也一样。
func TestSend_RejectedDoesNotRetry(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusUnprocessableEntity,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":"invalid_field","message":"cpu_pct 超出范围"}`))
			}))
			defer srv.Close()

			c := mustClient(t, Options{ServerURL: srv.URL, Node: "web01", Token: "t"})
			err := c.Send(context.Background(), sampleReport())

			var rejected *RejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("期望 RejectedError，得到 %v", err)
			}
			if rejected.Status != status {
				t.Errorf("Status = %d，期望 %d", rejected.Status, status)
			}
			if rejected.Code != "invalid_field" {
				t.Errorf("Code = %q，期望 invalid_field", rejected.Code)
			}
			if rejected.Message == "" {
				t.Error("Message 不应为空")
			}
			if n := atomic.LoadInt32(&calls); n != 1 {
				t.Errorf("4xx 不应重试，却请求了 %d 次", n)
			}
		})
	}
}

func TestSend_RetriesTransient(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusTooManyRequests,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := mustClient(t, Options{ServerURL: srv.URL, Node: "web01", Token: "t"})
			err := c.Send(context.Background(), sampleReport())

			if err == nil {
				t.Fatal("期望报错")
			}
			var rejected *RejectedError
			if errors.As(err, &rejected) {
				t.Errorf("临时错误不应被当成拒绝: %v", err)
			}
			if n := atomic.LoadInt32(&calls); n != 3 {
				t.Errorf("应尝试 3 次，实际 %d 次", n)
			}
		})
	}
}

func TestSend_BackoffGrows(t *testing.T) {
	var delays []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := mustClient(t, Options{
		ServerURL: srv.URL, Node: "web01", Token: "t",
		Attempts: 4, BaseDelay: time.Second, SleepFn: noSleep(&delays),
	})
	_ = c.Send(context.Background(), sampleReport())

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("退避次数 = %d，期望 %d（%v）", len(delays), len(want), delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("第 %d 次退避 = %v，期望 %v", i+1, delays[i], want[i])
		}
	}
}

func TestSend_SucceedsAfterTransientFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := mustClient(t, Options{ServerURL: srv.URL, Node: "web01", Token: "t"})
	if err := c.Send(context.Background(), sampleReport()); err != nil {
		t.Fatalf("重试后应成功，得到 %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("请求次数 = %d，期望 2", n)
	}
}

func TestSend_NetworkErrorIsRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关掉，制造连接失败

	c := mustClient(t, Options{ServerURL: url, Node: "web01", Token: "t"})
	err := c.Send(context.Background(), sampleReport())
	if err == nil {
		t.Fatal("期望报错")
	}
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		t.Errorf("网络错误不应被当成拒绝: %v", err)
	}
}

func TestSend_ContextCancelledDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := mustClient(t, Options{
		ServerURL: srv.URL, Node: "web01", Token: "t",
		SleepFn: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})

	err := c.Send(ctx, sampleReport())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("上下文取消应直接返回，得到 %v", err)
	}
}

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "缺 ServerURL", opts: Options{Node: "web01", Token: "t"}},
		{name: "缺 Node", opts: Options{ServerURL: "http://x", Token: "t"}},
		{name: "缺 Token", opts: Options{ServerURL: "http://x", Node: "web01"}},
		{name: "URL 无法解析", opts: Options{ServerURL: "://bad", Node: "web01", Token: "t"}},
		{name: "协议不是 http/https", opts: Options{ServerURL: "ftp://x", Node: "web01", Token: "t"}},
		{name: "URL 缺少主机名", opts: Options{ServerURL: "http://", Node: "web01", Token: "t"}},
		{name: "URL 只有路径", opts: Options{ServerURL: "/api", Node: "web01", Token: "t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Error("期望报错，却成功了")
			}
		})
	}
}

func TestNew_AppliesDefaults(t *testing.T) {
	c, err := New(Options{ServerURL: "http://x", Node: "web01", Token: "t"})
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	if c.opts.Attempts != 3 || c.opts.BaseDelay != time.Second || c.opts.Timeout != 10*time.Second {
		t.Errorf("默认值不对: %+v", c.opts)
	}
}

func TestBackoff(t *testing.T) {
	base := time.Second
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: base}, // 防御：attempt 小于 2 时按 base 处理
		{attempt: 2, want: base},
		{attempt: 3, want: 2 * base},
		{attempt: 4, want: 4 * base},
		{attempt: 6, want: 16 * base},
		{attempt: 50, want: 30 * time.Second}, // 上限
	}
	for _, tt := range tests {
		if got := backoff(base, tt.attempt); got != tt.want {
			t.Errorf("backoff(%v, %d) = %v，期望 %v", base, tt.attempt, got, tt.want)
		}
	}
}
