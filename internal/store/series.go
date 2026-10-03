package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// metricSpec 是白名单里的一项。
type metricSpec struct {
	Name string
	Expr string
}

// metricSpecs 同时是 SQL 白名单与对外暴露的指标清单，顺序即文档顺序。
//
// 请求里的 metric 只会被用来“查这张表”，永远不会被拼进 SQL——这是这一层
// 唯一的注入防线，所以不要把它改成任何形式的字符串拼接。
var metricSpecs = []metricSpec{
	{Name: "cpu_pct", Expr: "AVG(cpu_pct)"},
	// 内存百分比在查询时算，而不是存第二份数据：总量是常数，两个平均值的比
	// 与逐点求比的均值在这里没有实际差别，但前者少一列存储。
	{Name: "mem_pct", Expr: "100.0 * AVG(mem_used) / AVG(mem_total)"},
	{Name: "load1", Expr: "AVG(load1)"},
	{Name: "load5", Expr: "AVG(load5)"},
	{Name: "load15", Expr: "AVG(load15)"},
}

var metricIndex = func() map[string]string {
	m := make(map[string]string, len(metricSpecs))
	for _, s := range metricSpecs {
		m[s.Name] = s.Expr
	}
	return m
}()

// ErrUnknownMetric 表示请求的指标不在白名单里。
var ErrUnknownMetric = errors.New("store: 未知指标")

// Metrics 返回可查询的指标名，顺序固定。
func Metrics() []string {
	out := make([]string, 0, len(metricSpecs))
	for _, s := range metricSpecs {
		out = append(out, s.Name)
	}
	return out
}

// SeriesPoint 是一个降采样后的点。
type SeriesPoint struct {
	TS    int64
	Value float64
}

// SeriesQuery 是一次曲线查询的参数。
type SeriesQuery struct {
	NodeID string
	Metric string
	From   time.Time
	To     time.Time
	// Step 是降采样步长（秒），必须为正。
	Step int64
}

// Series 返回按 step 分桶平均后的曲线。
//
// 刻意不做预聚合表：30 秒 × 30 天 = 86,400 行/节点，SQLite 直接分组就够快，
// 而多一张聚合表就多一套失效与回填逻辑（规格 §2 取舍 3、§4.3）。
func (s *Store) Series(q SeriesQuery) ([]SeriesPoint, error) {
	expr, ok := metricIndex[q.Metric]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMetric, q.Metric)
	}
	if q.Step <= 0 {
		return nil, errors.New("store: step 必须为正")
	}

	// expr 来自白名单常量；两个 step 是绑定参数。
	query := `SELECT (ts / ?) * ? AS bucket, ` + expr + ` AS v
		FROM sample
		WHERE node_id = ? AND ts >= ? AND ts <= ?
		GROUP BY bucket ORDER BY bucket`

	rows, err := s.db.Query(query, q.Step, q.Step, q.NodeID, q.From.Unix(), q.To.Unix())
	if err != nil {
		return nil, fmt.Errorf("查询曲线: %w", err)
	}
	defer rows.Close()

	var out []SeriesPoint
	for rows.Next() {
		var bucket int64
		var v sql.NullFloat64
		if err := rows.Scan(&bucket, &v); err != nil {
			return nil, fmt.Errorf("解析曲线点: %w", err)
		}
		// 整桶全为 NULL 时跳过，而不是塞一个 0——0 和“没有数据”在图上
		// 是完全不同的两件事。
		if !v.Valid {
			continue
		}
		out = append(out, SeriesPoint{TS: bucket, Value: v.Float64})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历曲线: %w", err)
	}
	return out, nil
}

// LastInterval 返回节点最近一次申报的上报间隔（秒），第二个返回值表示有无记录。
func (s *Store) LastInterval(nodeID string) (int, bool) {
	var v int
	err := s.db.QueryRow(`SELECT last_interval FROM node_state WHERE node_id = ?`, nodeID).Scan(&v)
	if err != nil {
		// 包含 sql.ErrNoRows：从未上报过的节点没有记录，这不是错误。
		return 0, false
	}
	return v, true
}

// maxSeriesPoints 是一条曲线的目标点数上限。
const maxSeriesPoints = 500

// stepLadder 是候选步长（秒）。
//
// 用固定阶梯而不是任意整数，坐标轴刻度与前端缓存才整齐。
var stepLadder = []int64{30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, 86400}

// ChooseStep 选一个降采样步长，让点数不超过 maxSeriesPoints。
//
// minStep 由调用方给出（节点申报的上报间隔）：比原始粒度更细的步长没有意义。
func ChooseStep(spanSeconds, minStep int64) int64 {
	if minStep <= 0 {
		minStep = 1
	}
	if spanSeconds <= 0 {
		return minStep
	}

	want := spanSeconds / maxSeriesPoints
	step := stepLadder[len(stepLadder)-1]
	for _, s := range stepLadder {
		if s >= want {
			step = s
			break
		}
	}
	if step < minStep {
		step = minStep
	}
	return step
}
