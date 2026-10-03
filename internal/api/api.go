// Package api 实现 server 的 HTTP 入口。
//
// 只有三条路由：收上报、发查询、自检。刻意没有别的——系统里不存在任何
// 向 agent 下发指令的代码路径（规格 §1）。
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/store"
)

// Options 是 API 层的配置。
type Options struct {
	// ReportPerMinute 是单节点每分钟的上报次数上限，默认 10。
	ReportPerMinute int
	// Logger 留空则用 slog.Default()。
	Logger *slog.Logger
	// Now 仅供测试注入。
	Now func() time.Time
}

// Server 是 HTTP 处理器集合。
type Server struct {
	store   *store.Store
	logger  *slog.Logger
	now     func() time.Time
	limiter *limiter
}

// New 构造 API 层。
func New(st *store.Store, opts Options) *Server {
	if opts.ReportPerMinute <= 0 {
		opts.ReportPerMinute = 10
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{
		store:   st,
		logger:  opts.Logger,
		now:     opts.Now,
		limiter: newLimiter(opts.ReportPerMinute, time.Minute, opts.Now),
	}
}

// Handler 返回全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/report", s.handleReport)
	mux.HandleFunc("GET /api/v1/nodes", s.handleNodes)
	mux.HandleFunc("GET /api/v1/series", s.handleSeries)
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	return mux
}

// ---------- 上报 ----------

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, protocol.ErrorResponse{
			Code:    "unsupported_media_type",
			Message: "Content-Type 必须是 application/json",
		})
		return
	}

	rep, err := protocol.DecodeStrict(r.Body)
	if err != nil {
		s.rejectPayload(w, err)
		return
	}

	// 身份：node 必须存在于配置，且 token 匹配。
	// “节点不存在”与“token 不对”回同一个 401，不泄露某个 id 是否存在。
	node, ok := s.store.Node(rep.Node)
	if !ok || !tokenMatches(r.Header.Get("Authorization"), node.Token) {
		writeError(w, http.StatusUnauthorized, protocol.ErrorResponse{
			Code:    "unauthorized",
			Message: "节点不存在或 token 不匹配",
		})
		return
	}

	// 频率按节点限流，不按 IP：agent 很可能都在同一个出口或反向代理后面。
	if !s.limiter.allow(rep.Node) {
		writeError(w, http.StatusTooManyRequests, protocol.ErrorResponse{
			Code:    "rate_limited",
			Message: "上报过于频繁",
		})
		return
	}

	if err := s.store.Record(rep, s.now()); err != nil {
		// 内部错误只进日志，不回显给对端。
		s.logger.Error("写入上报失败", "node", rep.Node, "err", err)
		writeError(w, http.StatusInternalServerError, protocol.ErrorResponse{
			Code:    "internal_error",
			Message: "写入失败",
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rejectPayload 把校验错误映射成响应。
//
// 详细原因只进服务端日志；给对端的描述是 server 自己构造的短文本，
// 不含请求原文。
func (s *Server) rejectPayload(w http.ResponseWriter, err error) {
	var ve *protocol.ValidationError
	if !errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, protocol.ErrorResponse{
			Code:    "malformed_json",
			Message: "载荷无法解析",
		})
		return
	}

	s.logger.Warn("拒绝上报", "code", ve.Code, "field", ve.Field, "detail", ve.Msg)
	writeError(w, ve.HTTPStatus(), protocol.ErrorResponse{
		Code:    string(ve.Code),
		Field:   ve.Field,
		Message: ve.Msg,
	})
}

// ---------- 查询 ----------

// NodesResponse 是 GET /api/v1/nodes 的响应体，也是前端的全部输入。
type NodesResponse struct {
	GeneratedAt int64      `json:"generated_at"`
	Nodes       []NodeView `json:"nodes"`
}

// NodeView 是单个节点对外暴露的全部字段。
//
// 这是一份白名单：数据库里还有什么、agent 报了什么，都不会直接漏出去。
type NodeView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	// LastSeen/FirstSeen 为 null 表示该节点从未上报过。
	LastSeen  *int64             `json:"last_seen"`
	FirstSeen *int64             `json:"first_seen"`
	IntervalS int                `json:"interval_s"`
	Uptime    map[string]float64 `json:"uptime"`
	// Metrics 为 null 表示从未上报，前端应显示“从未上线”。
	Metrics *MetricsView `json:"metrics"`
}

// MetricsView 是最近一次上报里对前端有意义的部分。
type MetricsView struct {
	Load         []float64  `json:"load"`
	CPUPct       float64    `json:"cpu_pct"`
	Mem          MemView    `json:"mem"`
	Disk         []DiskView `json:"disk"`
	UptimeS      int64      `json:"uptime_s"`
	AgentVersion string     `json:"agent_version"`
}

type MemView struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

type DiskView struct {
	Mount string `json:"mount"`
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	now := s.now()

	statuses, err := s.store.Snapshot(now)
	if err != nil {
		s.logger.Error("读取节点快照失败", "err", err)
		writeError(w, http.StatusInternalServerError, protocol.ErrorResponse{
			Code: "internal_error", Message: "读取失败",
		})
		return
	}

	resp := NodesResponse{GeneratedAt: now.Unix(), Nodes: make([]NodeView, 0, len(statuses))}
	for _, st := range statuses {
		v := NodeView{
			ID:        st.NodeID,
			Name:      st.DisplayName,
			Online:    st.Online,
			IntervalS: st.IntervalS,
			Uptime:    make(map[string]float64, len(st.Rates)),
		}
		if !st.FirstSeen.IsZero() {
			t := st.FirstSeen.Unix()
			v.FirstSeen = &t
		}
		if !st.LastSeen.IsZero() {
			t := st.LastSeen.Unix()
			v.LastSeen = &t
		}
		for name, rate := range st.Rates {
			v.Uptime[name] = rate
		}
		if st.Report != nil {
			v.Metrics = metricsView(st.Report)
		}
		resp.Nodes = append(resp.Nodes, v)
	}

	// 数据本来就是 30 秒粒度，缓存 15 秒零损失，却能挡掉大量重复请求。
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, resp)
}

func metricsView(rep *protocol.Report) *MetricsView {
	disk := make([]DiskView, 0, len(rep.Disk))
	for _, d := range rep.Disk {
		disk = append(disk, DiskView{Mount: d.Mount, Used: d.Used, Total: d.Total})
	}
	load := make([]float64, len(rep.Load))
	copy(load, rep.Load)

	return &MetricsView{
		Load:         load,
		CPUPct:       rep.CPUPct,
		Mem:          MemView{Used: rep.Mem.Used, Total: rep.Mem.Total},
		Disk:         disk,
		UptimeS:      rep.UptimeS,
		AgentVersion: rep.AgentVersion,
	}
}

// ---------- 自检 ----------

// HealthResponse 是 GET /api/v1/health 的响应体。
type HealthResponse struct {
	Status string `json:"status"`
	Nodes  int    `json:"nodes"`
	Online int    `json:"online"`
	Time   int64  `json:"time"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	now := s.now()

	resp := HealthResponse{Status: "ok", Time: now.Unix()}
	statuses, err := s.store.Snapshot(now)
	if err != nil {
		s.logger.Error("自检读取快照失败", "err", err)
		resp.Status = "degraded"
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	resp.Nodes = len(statuses)
	for _, st := range statuses {
		if st.Online {
			resp.Online++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, body protocol.ErrorResponse) {
	body.Message = sanitize(body.Message)
	body.Field = sanitize(body.Field)
	writeJSON(w, status, body)
}

// sanitize 换掉控制字符并截断。
//
// 错误描述里可能带上对方提供的字段名（未知字段就是这种情况），它最终会进入
// 别人的终端或日志。JSON 编码本身是安全的，但没必要把控制字符送到下游。
func sanitize(s string) string {
	const maxLen = 200

	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxLen {
			break
		}
		if r < 0x20 || r == 0x7f {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isJSON 判断 Content-Type 是否为 application/json（忽略参数与大小写）。
func isJSON(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// tokenMatches 做常量时间比较。长度不同会立刻返回 false，这个信息量可以接受。
func tokenMatches(authorization, want string) bool {
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(authorization[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---------- 限流 ----------

// limiter 是按 key 的固定窗口计数器。
//
// 固定窗口（而不是滑动窗口或令牌桶）是刻意的：上报频率是分钟级的，
// 窗口边界处的双倍突发对本场景毫无影响，换来的是十行可读代码。
type limiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	now     func() time.Time
	resetAt time.Time
	hits    map[string]int
}

func newLimiter(limit int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{limit: limit, window: window, now: now, hits: make(map[string]int)}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.resetAt.IsZero() || !now.Before(l.resetAt) {
		l.resetAt = now.Add(l.window)
		clear(l.hits)
	}
	l.hits[key]++
	return l.hits[key] <= l.limit
}
