package store

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
)

func TestChooseStep(t *testing.T) {
	tests := []struct {
		name     string
		span     int64
		minStep  int64
		wantStep int64
	}{
		{name: "一分钟跨度取下限", span: 60, minStep: 30, wantStep: 30},
		{name: "一小时跨度", span: 3600, minStep: 30, wantStep: 30},       // 3600/500 = 7.2 → 30
		{name: "六小时跨度", span: 21600, minStep: 30, wantStep: 60},      // 43.2 → 60
		{name: "24 小时跨度", span: 86400, minStep: 30, wantStep: 300},   // 172.8 → 300
		{name: "7 天跨度", span: 604800, minStep: 30, wantStep: 1800},   // 1209.6 → 1800
		{name: "30 天跨度", span: 2592000, minStep: 30, wantStep: 7200}, // 5184 → 7200
		// 100 天 / 500 = 17280 秒，阶梯里第一个不小于它的是 21600。
		{name: "跨度大于大多数阶梯值", span: 100 * 86400, minStep: 30, wantStep: 21600},
		{name: "节点间隔大于阶梯值", span: 3600, minStep: 7200, wantStep: 7200},
		{name: "零跨度返回 minStep", span: 0, minStep: 30, wantStep: 30},
		{name: "负跨度返回 minStep", span: -1, minStep: 60, wantStep: 60},
		{name: "minStep 非法时兜底为 1", span: 100, minStep: 0, wantStep: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ChooseStep(tt.span, tt.minStep); got != tt.wantStep {
				t.Errorf("ChooseStep(%d, %d) = %d，期望 %d", tt.span, tt.minStep, got, tt.wantStep)
			}
		})
	}
}

func TestMetrics_OrderStable(t *testing.T) {
	want := []string{"cpu_pct", "mem_pct", "load1", "load5", "load15"}
	got := Metrics()
	if len(got) != len(want) {
		t.Fatalf("指标数 = %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个指标 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

func TestSeries_Downsampling(t *testing.T) {
	s := newStore(t, testNode("web01"))
	base := time.Unix(1_700_000_000, 0)

	// 每 30 秒一条，共 10 条：CPU 依次 10,20,...,100。
	for i := 0; i < 10; i++ {
		rep := testReport("web01", 30)
		rep.CPUPct = float64((i + 1) * 10)
		if err := s.Record(rep, base.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}

	from := base
	to := base.Add(10 * time.Minute)

	// 不分桶（step=30）：应拿到 10 个点，值就是原值。
	points, err := s.Series(SeriesQuery{NodeID: "web01", Metric: "cpu_pct", From: from, To: to, Step: 30})
	if err != nil {
		t.Fatalf("Series() 失败: %v", err)
	}
	if len(points) != 10 {
		t.Fatalf("点数 = %d，期望 10", len(points))
	}
	if points[0].Value != 10 || points[9].Value != 100 {
		t.Errorf("首尾值 = %v / %v，期望 10 / 100", points[0].Value, points[9].Value)
	}

	// 每 5 分钟一桶（step=300）。base 并不落在 5 分钟边界上
	//（1700000000 % 300 == 200），所以前四条落在前一个桶、其余六条落在
	// 后一个桶——这顺带验证了 (ts/step)*step 的边界行为，而不是想当然地
	// 以为“十条都在一个桶里”。
	points, err = s.Series(SeriesQuery{NodeID: "web01", Metric: "cpu_pct", From: from, To: to, Step: 300})
	if err != nil {
		t.Fatalf("Series() 失败: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("5 分钟分桶后点数 = %d，期望 2", len(points))
	}
	// 第一个桶：10,20,30,40 → 25；第二个桶：50..100 → 75。
	wantAvg := []float64{25, 75}
	for i, want := range wantAvg {
		if math.Abs(points[i].Value-want) > 1e-9 {
			t.Errorf("第 %d 个桶的均值 = %v，期望 %v", i, points[i].Value, want)
		}
	}
	// 桶的时间戳必须是步长的整数倍。
	for _, p := range points {
		if p.TS%300 != 0 {
			t.Errorf("桶时间戳 %d 不是 300 的整数倍", p.TS)
		}
	}
}

func TestSeries_MemoryPercent(t *testing.T) {
	s := newStore(t, testNode("web01"))
	at := time.Unix(1_700_000_000, 0)

	rep := testReport("web01", 30)
	rep.Mem = protocol.Mem{Used: 2_000, Total: 4_000} // 50%
	if err := s.Record(rep, at); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}

	points, err := s.Series(SeriesQuery{NodeID: "web01", Metric: "mem_pct", From: at.Add(-time.Minute), To: at.Add(time.Minute), Step: 30})
	if err != nil {
		t.Fatalf("Series() 失败: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("点数 = %d，期望 1", len(points))
	}
	if math.Abs(points[0].Value-50) > 1e-9 {
		t.Errorf("内存百分比 = %v，期望 50（内存百分比是查询时算的）", points[0].Value)
	}
}

func TestSeries_RangeIsInclusive(t *testing.T) {
	s := newStore(t, testNode("web01"))
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < 5; i++ {
		if err := s.Record(testReport("web01", 30), base.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}

	// 只取中间两个点。
	points, err := s.Series(SeriesQuery{
		NodeID: "web01", Metric: "cpu_pct",
		From: base.Add(30 * time.Second), To: base.Add(60 * time.Second), Step: 30,
	})
	if err != nil {
		t.Fatalf("Series() 失败: %v", err)
	}
	if len(points) != 2 {
		t.Errorf("点数 = %d，期望 2（from/to 都是闭区间）", len(points))
	}
}

func TestSeries_EmptyResult(t *testing.T) {
	s := newStore(t, testNode("web01"))

	points, err := s.Series(SeriesQuery{
		NodeID: "web01", Metric: "cpu_pct",
		From: time.Unix(1_700_000_000, 0), To: time.Unix(1_700_003_600, 0), Step: 30,
	})
	if err != nil {
		t.Fatalf("Series() 失败: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("点数 = %d，期望 0", len(points))
	}
}

func TestSeries_RejectsUnknownMetric(t *testing.T) {
	s := newStore(t, testNode("web01"))

	for _, metric := range []string{"", "cpu", "cpu_pct; DROP TABLE sample", "mem_used"} {
		_, err := s.Series(SeriesQuery{
			NodeID: "web01", Metric: metric,
			From: time.Unix(1_700_000_000, 0), To: time.Unix(1_700_003_600, 0), Step: 30,
		})
		if !errors.Is(err, ErrUnknownMetric) {
			t.Errorf("metric=%q 应返回 ErrUnknownMetric，得到 %v", metric, err)
		}
	}
}

func TestSeries_RejectsNonPositiveStep(t *testing.T) {
	s := newStore(t, testNode("web01"))

	for _, step := range []int64{0, -30} {
		if _, err := s.Series(SeriesQuery{
			NodeID: "web01", Metric: "cpu_pct",
			From: time.Unix(1_700_000_000, 0), To: time.Unix(1_700_003_600, 0), Step: step,
		}); err == nil {
			t.Errorf("step=%d 应报错", step)
		}
	}
}

func TestLastInterval(t *testing.T) {
	s := newStore(t, testNode("web01"))

	if _, ok := s.LastInterval("web01"); ok {
		t.Error("从未上报过的节点不应有间隔记录")
	}

	rep := testReport("web01", 45)
	if err := s.Record(rep, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}

	got, ok := s.LastInterval("web01")
	if !ok || got != 45 {
		t.Errorf("LastInterval() = %d, %v，期望 45, true", got, ok)
	}
}
