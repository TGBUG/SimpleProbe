// Package push 把上报载荷送到 server，并区分“该重试”与“不该重试”的错误。
//
// 分类规则来自规格 §4.7：
//   - 网络错误 / 5xx → 暂时性问题，指数退避重试；
//   - 429 限流 → **不重试**，当轮放弃（理由见 RateLimitedError）；
//   - 其余 4xx → server 明确拒绝，重试一万次结果也一样，必须立刻上报给运维。
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

// RejectedError 表示 server 明确拒绝了这条载荷（4xx）。
//
// 它意味着“我发的东西不对”——配置错了、协议版本不匹配、或者 agent 有 bug。
type RejectedError struct {
	Status  int
	Code    string
	Message string
}

func (e *RejectedError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("server 拒绝上报（HTTP %d, %s）：%s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("server 拒绝上报（HTTP %d）：%s", e.Status, e.Message)
}

// RateLimitedError 表示 server 因为超过频率上限而拒绝了这次上报（HTTP 429）。
//
// 它和 RejectedError 一样不该重试，但原因完全不同：载荷本身没问题，只是这个
// 时间窗口的额度用完了。
//
// **为什么不能重试**：限流窗口是整整一分钟，而单周期的重试是在几秒内打完的——
// 那些重试不可能成功，反而会继续消耗同一个计数器，把节点往坑里推得更深。
// 更糟的是它会自我维持：额度一满，每次重试都让它更满。
// 下一个上报周期本来就会重来，那时窗口已经滚动，所以当轮直接放弃。
type RateLimitedError struct {
	Message string
}

func (e *RateLimitedError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("被 server 限流（HTTP 429）：%s", e.Message)
	}
	return "被 server 限流（HTTP 429）"
}

// Options 是上报客户端的配置。
type Options struct {
	ServerURL string        // 例如 https://probe.example.com
	Node      string        // 节点 id，必须与 server 配置一致
	Token     string        // 明文 token
	Timeout   time.Duration // 单次请求超时，默认 10s
	Attempts  int           // 单个周期内的最大尝试次数，默认 3
	BaseDelay time.Duration // 退避基数，默认 1s

	// HTTPClient 留空则使用带 Timeout 的默认客户端。
	HTTPClient *http.Client

	// SleepFn 仅供测试注入，留空使用可被 ctx 取消的睡眠。
	SleepFn func(ctx context.Context, d time.Duration) error
}

// Client 是上报客户端。它是并发安全的。
type Client struct {
	opts      Options
	client    *http.Client
	reportURL string
}

// New 构造上报客户端。
func New(opts Options) (*Client, error) {
	switch {
	case opts.ServerURL == "":
		return nil, errors.New("push: ServerURL 不能为空")
	case opts.Node == "":
		return nil, errors.New("push: Node 不能为空")
	case opts.Token == "":
		return nil, errors.New("push: Token 不能为空")
	}
	// 在启动时就把坏配置挡住，而不是等到每次上报才失败——
	// 否则运维只会看到“上报失败”而不知道是 URL 写错了。
	u, err := url.Parse(opts.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("push: ServerURL 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("push: ServerURL 必须是 http/https，得到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("push: ServerURL 缺少主机名")
	}

	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Attempts <= 0 {
		opts.Attempts = 3
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = time.Second
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{
		opts:      opts,
		client:    client,
		reportURL: strings.TrimRight(opts.ServerURL, "/") + "/api/v1/report",
	}, nil
}

// Send 发送一次上报。
//
// 遇到 4xx 会立即返回 RejectedError 且不再重试；其余错误最多尝试
// opts.Attempts 次后返回最后一次的错误。
func (c *Client) Send(ctx context.Context, rep *protocol.Report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("序列化上报载荷: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= c.opts.Attempts; attempt++ {
		if attempt > 1 {
			if err := c.sleep(ctx, backoff(c.opts.BaseDelay, attempt)); err != nil {
				return err
			}
		}
		lastErr = c.once(ctx, body)
		if lastErr == nil {
			return nil
		}
		var rejected *RejectedError
		if errors.As(lastErr, &rejected) {
			// 4xx：重试没有意义，立刻向上报。
			return lastErr
		}
		var limited *RateLimitedError
		if errors.As(lastErr, &limited) {
			// 429：窗口没滚之前重试不可能成功，还会继续消耗额度。
			return lastErr
		}
	}
	return fmt.Errorf("上报失败，已尝试 %d 次: %w", c.opts.Attempts, lastErr)
}

func (c *Client) once(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.reportURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	req.Header.Set("User-Agent", "probe-agent")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 只读少量响应体用于诊断，防止对端返回巨大内容把 agent 拖垮。
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		var e protocol.ErrorResponse
		_ = json.Unmarshal(payload, &e)
		return &RateLimitedError{Message: e.Message}
	case resp.StatusCode >= 500:
		return fmt.Errorf("server 内部错误（HTTP %d）", resp.StatusCode)
	default:
		var e protocol.ErrorResponse
		_ = json.Unmarshal(payload, &e)
		return &RejectedError{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
	}
}

// backoff 返回第 attempt 次尝试前的等待时间。
//
// attempt 从 2 开始（第一次失败之后才需要等），因此 attempt=2 得到 base，
// attempt=3 得到 2×base，以此类推，上限 30 秒。
//
// 刻意不加抖动：节点只有个位数，不存在惊群问题；加了抖动反而让测试变随机。
func backoff(base time.Duration, attempt int) time.Duration {
	shift := attempt - 2
	if shift < 0 {
		shift = 0
	}
	if shift > 10 {
		shift = 10
	}
	d := base << shift
	if d > 30*time.Second || d <= 0 {
		d = 30 * time.Second
	}
	return d
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.opts.SleepFn != nil {
		return c.opts.SleepFn(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
