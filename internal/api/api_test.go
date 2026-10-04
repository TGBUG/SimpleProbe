package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/config"
	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/store"
)

var testNow = time.Unix(1_700_000_000, 0)

func testNodes() []config.Node {
	return []config.Node{{ID: "web01", DisplayName: "Web 01", Token: "s3cret"}}
}

func newTestServer(t *testing.T, opts Options, nodes ...config.Node) *Server {
	t.Helper()
	if len(nodes) == 0 {
		nodes = testNodes()
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"), nodes)
	if err != nil {
		t.Fatalf("store.Open() 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if opts.Now == nil {
		opts.Now = func() time.Time { return testNow }
	}
	return New(st, opts)
}

func validBody(t *testing.T) []byte {
	t.Helper()
	rep := protocol.Report{
		V: 1, Node: "web01", IntervalS: 30,
		Load:    []float64{0.4, 0.5, 0.6},
		CPUPct:  12.3,
		Mem:     protocol.Mem{Used: 100, Total: 200},
		Disk:    []protocol.Disk{{Mount: "/", Used: 1, Total: 2}},
		UptimeS: 42, AgentVersion: "0.1.0",
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("序列化载荷失败: %v", err)
	}
	return b
}

func postReport(srv *Server, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e protocol.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("响应不是错误信封: %q", w.Body.String())
	}
	return e.Code
}

func TestReport_HappyPath(t *testing.T) {
	srv := newTestServer(t, Options{})

	w := postReport(srv, validBody(t), "s3cret")
	if w.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d，期望 204（body: %s）", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 不应带响应体，得到 %q", w.Body.String())
	}

	// 落库了才算真的成功。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var resp NodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 /nodes 响应失败: %v", err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Metrics == nil {
		t.Fatalf("节点状态不对: %+v", resp.Nodes)
	}
	if resp.Nodes[0].Metrics.CPUPct != 12.3 {
		t.Errorf("cpu_pct = %v", resp.Nodes[0].Metrics.CPUPct)
	}
	if !resp.Nodes[0].Online {
		t.Error("刚上报过应显示在线")
	}
}

func TestReport_ContentType(t *testing.T) {
	srv := newTestServer(t, Options{})

	for _, ct := range []string{"", "text/plain", "application/xml"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/report", bytes.NewReader(validBody(t)))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Authorization", "Bearer s3cret")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type=%q 时状态码 = %d，期望 415", ct, w.Code)
		}
	}

	// 带参数与大小写差异的写法必须被接受。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/report", bytes.NewReader(validBody(t)))
	req.Header.Set("Content-Type", "Application/JSON; charset=utf-8")
	req.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("带参数的 Content-Type 应被接受，得到 %d", w.Code)
	}
}

// TestReport_AuthFailuresAreIndistinguishable 钉住“不泄露节点是否存在”：
// 未知节点与错误 token 必须返回一模一样的响应。
func TestReport_AuthFailuresAreIndistinguishable(t *testing.T) {
	srv := newTestServer(t, Options{})

	unknown := postReport(srv, validBody(t), "s3cret") // node 存在，token 对——对照组先跑通
	if unknown.Code != http.StatusNoContent {
		t.Fatalf("对照组应该成功，得到 %d", unknown.Code)
	}

	badToken := postReport(srv, validBody(t), "wrong")

	var rep protocol.Report
	if err := json.Unmarshal(validBody(t), &rep); err != nil {
		t.Fatalf("解析载荷失败: %v", err)
	}
	// 必须用合法的 slug：否则会在身份校验之前就被取值校验挡下（400），
	// 那测的就不是“未知节点”这条路径了。
	rep.Node = "ghost"
	body, _ := json.Marshal(rep)
	unknownNode := postReport(srv, body, "s3cret")

	if badToken.Code != http.StatusUnauthorized || unknownNode.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d / %d，都应为 401", badToken.Code, unknownNode.Code)
	}
	if badToken.Body.String() != unknownNode.Body.String() {
		t.Errorf("两种失败模式的响应体不同，会泄露节点是否存在：\n%q\n%q",
			badToken.Body.String(), unknownNode.Body.String())
	}
}

func TestReport_MissingAuthorization(t *testing.T) {
	srv := newTestServer(t, Options{})
	if w := postReport(srv, validBody(t), ""); w.Code != http.StatusUnauthorized {
		t.Errorf("缺少 Authorization 时状态码 = %d，期望 401", w.Code)
	}
}

func TestReport_InvalidPayload(t *testing.T) {
	srv := newTestServer(t, Options{})

	tests := []struct {
		name     string
		body     string
		wantCode string
		wantHTTP int
	}{
		{name: "空体", body: "", wantCode: "malformed_json", wantHTTP: 400},
		{name: "语法错误", body: `{"v":1,`, wantCode: "malformed_json", wantHTTP: 400},
		{name: "未知字段", body: `{"v":1,"node":"web01","interval_s":30,"load":[1,2,3],"cpu_pct":1,"mem":{"used":1,"total":2},"disk":[{"mount":"/","used":1,"total":2}],"uptime_s":1,"agent_version":"1","x":1}`,
			wantCode: "unknown_field", wantHTTP: 400},
		{name: "版本不符", body: `{"v":99,"node":"web01","interval_s":30,"load":[1,2,3],"cpu_pct":1,"mem":{"used":1,"total":2},"disk":[{"mount":"/","used":1,"total":2}],"uptime_s":1,"agent_version":"1"}`,
			wantCode: "unsupported_version", wantHTTP: 400},
		{name: "取值越界", body: `{"v":1,"node":"web01","interval_s":30,"load":[1,2,3],"cpu_pct":101,"mem":{"used":1,"total":2},"disk":[{"mount":"/","used":1,"total":2}],"uptime_s":1,"agent_version":"1"}`,
			wantCode: "invalid_field", wantHTTP: 400},
		{name: "尾随内容", body: string(validBody(t)) + "{}", wantCode: "trailing_data", wantHTTP: 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postReport(srv, []byte(tt.body), "s3cret")
			if w.Code != tt.wantHTTP {
				t.Fatalf("状态码 = %d，期望 %d（body: %s）", w.Code, tt.wantHTTP, w.Body.String())
			}
			if got := errorCode(t, w); got != tt.wantCode {
				t.Errorf("错误码 = %q，期望 %q", got, tt.wantCode)
			}
		})
	}
}

func TestReport_BodyTooLarge(t *testing.T) {
	srv := newTestServer(t, Options{})

	body := append(validBody(t), bytes.Repeat([]byte(" "), protocol.MaxBodyBytes)...)
	w := postReport(srv, body, "s3cret")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", w.Code)
	}
	if got := errorCode(t, w); got != "body_too_large" {
		t.Errorf("错误码 = %q", got)
	}
}

func TestReport_RateLimited(t *testing.T) {
	srv := newTestServer(t, Options{ReportPerMinute: 3})

	for i := 0; i < 3; i++ {
		if w := postReport(srv, validBody(t), "s3cret"); w.Code != http.StatusNoContent {
			t.Fatalf("第 %d 次上报应成功，得到 %d", i+1, w.Code)
		}
	}
	w := postReport(srv, validBody(t), "s3cret")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超出上限后状态码 = %d，期望 429", w.Code)
	}
	if got := errorCode(t, w); got != "rate_limited" {
		t.Errorf("错误码 = %q", got)
	}
}

// TestReport_LimitIsPerNode 确认限流不会因为一个节点刷满而误伤别人。
func TestReport_LimitIsPerNode(t *testing.T) {
	nodes := []config.Node{
		{ID: "web01", DisplayName: "Web 01", Token: "t1"},
		{ID: "nas", DisplayName: "NAS", Token: "t2"},
	}
	srv := newTestServer(t, Options{ReportPerMinute: 2}, nodes...)

	body := func(node string) []byte {
		rep := protocol.Report{
			V: 1, Node: node, IntervalS: 30,
			Load: []float64{1, 1, 1}, CPUPct: 1,
			Mem: protocol.Mem{Used: 1, Total: 2}, Disk: []protocol.Disk{{Mount: "/", Used: 1, Total: 2}},
			UptimeS: 1, AgentVersion: "0.1.0",
		}
		b, _ := json.Marshal(rep)
		return b
	}

	for i := 0; i < 2; i++ {
		if w := postReport(srv, body("web01"), "t1"); w.Code != http.StatusNoContent {
			t.Fatalf("web01 第 %d 次应成功，得到 %d", i+1, w.Code)
		}
	}
	if w := postReport(srv, body("web01"), "t1"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("web01 应被限流，得到 %d", w.Code)
	}
	// 另一个节点不受影响。
	if w := postReport(srv, body("nas"), "t2"); w.Code != http.StatusNoContent {
		t.Errorf("nas 不应被 web01 的限流影响，得到 %d", w.Code)
	}
}

func TestNodes_NeverReportedNode(t *testing.T) {
	srv := newTestServer(t, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=15" {
		t.Errorf("Cache-Control = %q", cc)
	}

	var resp NodesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("节点数 = %d，期望 1（配置里有但从未上报的也要出现）", len(resp.Nodes))
	}

	n := resp.Nodes[0]
	if n.ID != "web01" || n.Name != "Web 01" {
		t.Errorf("身份字段不对: %+v", n)
	}
	if n.Metrics != nil {
		t.Error("从未上报时 metrics 应为 null")
	}
	if n.LastSeen != nil || n.FirstSeen != nil {
		t.Error("从未上报时 last_seen / first_seen 应为 null")
	}
	if n.Online {
		t.Error("不应显示在线")
	}
	for _, name := range []string{"24h", "7d", "30d"} {
		if _, ok := n.Uptime[name]; !ok {
			t.Errorf("缺少窗口 %s", name)
		}
	}
	// 显式确认 null 真的出现在 JSON 里，而不是被 omitempty 抹掉。
	if !strings.Contains(w.Body.String(), `"metrics":null`) {
		t.Errorf("响应里应显式出现 metrics:null，实际：%s", w.Body.String())
	}
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Status != "ok" || resp.Nodes != 1 || resp.Online != 0 {
		t.Errorf("自检结果不对: %+v", resp)
	}

	// 上报一条之后在线数应为 1。
	postReport(srv, validBody(t), "s3cret")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Online != 1 {
		t.Errorf("在线数 = %d，期望 1", resp.Online)
	}
}

func TestNodeView_DoesNotLeakToken(t *testing.T) {
	srv := newTestServer(t, Options{})
	postReport(srv, validBody(t), "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatal("公开接口泄露了 token")
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "普通文本不变", in: "cpu_pct 超出范围", want: "cpu_pct 超出范围"},
		{name: "控制字符被替换", in: "a\x00b\nc\x7fd", want: "a?b?c?d"},
		{name: "超长被截断", in: strings.Repeat("x", 500), want: strings.Repeat("x", 200)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("sanitize() = %q，期望 %q", got, tt.want)
			}
		})
	}
}

func TestIsJSON(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "application/json", want: true},
		{in: "application/json; charset=utf-8", want: true},
		{in: "Application/JSON", want: true},
		{in: " application/json ", want: true},
		{in: "text/plain", want: false},
		{in: "", want: false},
		{in: "application/jsonx", want: false},
	}
	for _, tt := range tests {
		if got := isJSON(tt.in); got != tt.want {
			t.Errorf("isJSON(%q) = %v，期望 %v", tt.in, got, tt.want)
		}
	}
}

func TestTokenMatches(t *testing.T) {
	tests := []struct {
		name string
		head string
		want string
		ok   bool
	}{
		{name: "正确", head: "Bearer abc", want: "abc", ok: true},
		{name: "前缀大小写无关", head: "bearer abc", want: "abc", ok: true},
		{name: "token 错误", head: "Bearer abd", want: "abc", ok: false},
		{name: "缺少 Bearer", head: "abc", want: "abc", ok: false},
		{name: "只有 Bearer", head: "Bearer ", want: "abc", ok: false},
		{name: "空头", head: "", want: "abc", ok: false},
		{name: "尾部空格被忽略", head: "Bearer abc  ", want: "abc", ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenMatches(tt.head, tt.want); got != tt.ok {
				t.Errorf("tokenMatches() = %v，期望 %v", got, tt.ok)
			}
		})
	}
}

func TestLimiter_WindowResets(t *testing.T) {
	now := testNow
	l := newLimiter(2, time.Minute, func() time.Time { return now })

	if !l.allow("a") || !l.allow("a") {
		t.Fatal("前两次应放行")
	}
	if l.allow("a") {
		t.Fatal("第三次应被拦下")
	}
	// 窗口翻页后重新计数。
	now = now.Add(time.Minute)
	if !l.allow("a") {
		t.Error("新窗口应重新放行")
	}
	// 不同 key 互不影响。
	if !l.allow("b") || !l.allow("b") || l.allow("b") {
		t.Error("key 之间不应互相影响")
	}
}

// TestDocs_ListsEveryMetric 防的是文档漂移。
//
// 可查询的指标清单同时存在于代码白名单（store.metricSpecs）和 docs/API.md 里。
// 白名单加了指标却忘了改文档，照着文档写前端的人就会永远漏掉它——而这类不一致
// 没有任何编译器或运行期信号会提醒。
func TestDocs_ListsEveryMetric(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "API.md"))
	if err != nil {
		t.Fatalf("读 docs/API.md 失败: %v", err)
	}
	text := string(doc)

	for _, m := range store.Metrics() {
		if !strings.Contains(text, "`"+m+"`") {
			t.Errorf("docs/API.md 里没有指标 %q —— 白名单加了指标就要同步文档", m)
		}
	}
}
