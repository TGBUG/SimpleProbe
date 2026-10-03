package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/store"
)

func newSeriesServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"), testNodes())
	if err != nil {
		t.Fatalf("store.Open() 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(st, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return testNow },
	})
	return srv, st
}

// seed 写入 n 条上报，最后一条落在 testNow，CPU 值依次是 10,20,...
func seed(t *testing.T, st *store.Store, n, intervalS int) {
	t.Helper()
	for i := 0; i < n; i++ {
		rep := &protocol.Report{
			V: 1, Node: "web01", IntervalS: intervalS,
			Load:    []float64{float64(i), float64(i) * 2, float64(i) * 3},
			CPUPct:  float64((i + 1) * 10),
			Mem:     protocol.Mem{Used: 250, Total: 1000},
			Disk:    []protocol.Disk{{Mount: "/", Used: 1, Total: 2}},
			UptimeS: 42, AgentVersion: "test",
		}
		at := testNow.Add(-time.Duration(n-1-i) * 30 * time.Second)
		if err := st.Record(rep, at); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}
}

func getSeries(srv *Server, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/series?"+query, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestSeries_HappyPath(t *testing.T) {
	srv, st := newSeriesServer(t)
	seed(t, st, 10, 30)

	from := testNow.Add(-270 * time.Second).Unix()
	w := getSeries(srv, fmt.Sprintf("node=web01&metric=cpu_pct&from=%d&to=%d&step=30", from, testNow.Unix()))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=15" {
		t.Errorf("Cache-Control = %q", cc)
	}

	var resp SeriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Node != "web01" || resp.Metric != "cpu_pct" || resp.Step != 30 {
		t.Errorf("元信息不对: %+v", resp)
	}
	if len(resp.Points) != 10 {
		t.Fatalf("点数 = %d，期望 10", len(resp.Points))
	}
	if resp.Points[0][1] != 10 || resp.Points[9][1] != 100 {
		t.Errorf("首尾值 = %v / %v，期望 10 / 100", resp.Points[0][1], resp.Points[9][1])
	}
	// 返回的时间戳是**桶起点**，未必等于 from：样本落在哪个桶取决于
	// (ts/step)*step。所以这里只断言它是 step 对齐的，且不早于 from-step。
	if ts := int64(resp.Points[0][0]); ts%30 != 0 || ts < from-30 || ts > from {
		t.Errorf("首点时间戳 = %d，期望是 30 的整数倍且落在 (%d, %d]", ts, from-30, from)
	}
	// 时间戳必须严格递增。
	for i := 1; i < len(resp.Points); i++ {
		if resp.Points[i][0] <= resp.Points[i-1][0] {
			t.Fatalf("第 %d 个点的时间戳没有递增: %v", i, resp.Points[i][0])
		}
	}
}

func TestSeries_Defaults(t *testing.T) {
	srv, st := newSeriesServer(t)
	seed(t, st, 3, 30)

	w := getSeries(srv, "node=web01")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}

	var resp SeriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Metric != "cpu_pct" {
		t.Errorf("默认指标 = %q，期望 cpu_pct", resp.Metric)
	}
	if resp.To != testNow.Unix() {
		t.Errorf("默认 to = %d，期望 %d", resp.To, testNow.Unix())
	}
	if want := testNow.Add(-24 * time.Hour).Unix(); resp.From != want {
		t.Errorf("默认 from = %d，期望 %d（最近 24 小时）", resp.From, want)
	}
}

func TestSeries_EmptyPointsIsArrayNotNull(t *testing.T) {
	srv, _ := newSeriesServer(t)

	w := getSeries(srv, "node=web01")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	// 空结果必须是 []，前端才不用多一个判空分支。
	if !strings.Contains(w.Body.String(), `"points":[]`) {
		t.Errorf("空结果应序列化成 []，实际：%s", w.Body.String())
	}
}

func TestSeries_UnknownNode(t *testing.T) {
	srv, _ := newSeriesServer(t)

	w := getSeries(srv, "node=ghost")
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}
	if got := errorCode(t, w); got != "unknown_node" {
		t.Errorf("错误码 = %q", got)
	}
}

func TestSeries_UnknownMetric(t *testing.T) {
	srv, st := newSeriesServer(t)
	seed(t, st, 3, 30)

	// 用 QueryEscape：直接拼进 URL 的话空格会让 HTTP 目标行解析失败，
	// 那不是我们想测的东西。
	w := getSeries(srv, "node=web01&metric="+url.QueryEscape("cpu_pct;DROP TABLE sample"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", w.Code)
	}

	var resp protocol.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Code != "invalid_field" || resp.Field != "metric" {
		t.Errorf("错误信封不对: %+v", resp)
	}
	// 描述里应列出可用指标，便于调用方自查。
	if !strings.Contains(resp.Message, "cpu_pct") {
		t.Errorf("描述应列出可选指标，实际 %q", resp.Message)
	}

	// 更要紧的：注入尝试之后表还在，写入照常成功。
	if err := st.Record(&protocol.Report{
		V: 1, Node: "web01", IntervalS: 30,
		Load: []float64{1, 1, 1}, CPUPct: 1,
		Mem: protocol.Mem{Used: 1, Total: 2}, Disk: []protocol.Disk{{Mount: "/", Used: 1, Total: 2}},
		UptimeS: 1, AgentVersion: "t",
	}, testNow); err != nil {
		t.Fatalf("注入尝试之后写入失败（表可能被删了）: %v", err)
	}
}

func TestSeries_BadRange(t *testing.T) {
	srv, _ := newSeriesServer(t)

	tests := []struct {
		name  string
		query string
	}{
		{name: "from 不是数字", query: "node=web01&from=abc"},
		{name: "to 不是数字", query: "node=web01&to=abc"},
		{name: "to 早于 from", query: "node=web01&from=200&to=100"},
		{name: "to 等于 from", query: "node=web01&from=100&to=100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := getSeries(srv, tt.query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400", w.Code)
			}
			if got := errorCode(t, w); got != "invalid_field" {
				t.Errorf("错误码 = %q", got)
			}
		})
	}
}

func TestSeries_BadStep(t *testing.T) {
	srv, _ := newSeriesServer(t)

	for _, step := range []string{"0", "-30", "abc"} {
		w := getSeries(srv, "node=web01&step="+step)
		if w.Code != http.StatusBadRequest {
			t.Errorf("step=%q 时状态码 = %d，期望 400", step, w.Code)
		}
	}
}

// TestSeries_ClampsSpan 钉住“跨度超过保留期就夹紧而不是报错”：
// 前端的 30 天按钮在保留期被调短时仍然要能用。
func TestSeries_ClampsSpan(t *testing.T) {
	srv, st := newSeriesServer(t)
	seed(t, st, 3, 30)

	from := testNow.Add(-60 * 24 * time.Hour).Unix()
	w := getSeries(srv, fmt.Sprintf("node=web01&from=%d&to=%d", from, testNow.Unix()))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}

	var resp SeriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if want := testNow.Add(-maxSeriesSpan).Unix(); resp.From != want {
		t.Errorf("from = %d，期望被夹到 %d（30 天前）", resp.From, want)
	}
}

// TestSeries_StepNeverFinerThanReportInterval 保证降采样不会比原始粒度更细。
func TestSeries_StepNeverFinerThanReportInterval(t *testing.T) {
	srv, st := newSeriesServer(t)
	seed(t, st, 3, 300) // 节点申报的上报间隔是 300 秒

	w := getSeries(srv, "node=web01&step=30")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}

	var resp SeriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Step != 300 {
		t.Errorf("step = %d，期望被抬到 300（节点上报间隔）", resp.Step)
	}
}

func TestSeries_ComputedMetrics(t *testing.T) {
	srv, st := newSeriesServer(t)
	seed(t, st, 1, 30) // 内存 250/1000

	tests := []struct {
		metric string
		want   float64
	}{
		{metric: "mem_pct", want: 25},
		{metric: "cpu_pct", want: 10},
		{metric: "load1", want: 0},
		{metric: "load5", want: 0},
		{metric: "load15", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.metric, func(t *testing.T) {
			w := getSeries(srv, "node=web01&metric="+tt.metric+"&step=30")
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d（%s）", w.Code, w.Body.String())
			}
			var resp SeriesResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(resp.Points) != 1 {
				t.Fatalf("点数 = %d，期望 1", len(resp.Points))
			}
			if resp.Points[0][1] != tt.want {
				t.Errorf("%s = %v，期望 %v", tt.metric, resp.Points[0][1], tt.want)
			}
		})
	}
}
