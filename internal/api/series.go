package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/store"
)

// maxSeriesSpan 是曲线查询允许的最大跨度。
//
// 明细只留 30 天，再往前也没有数据了；夹紧而不是报错，是为了让前端的
// “30 天”按钮在保留期被调短时仍然能用。响应里会回显实际生效的 from/to。
const maxSeriesSpan = 30 * 24 * time.Hour

// defaultSeriesSpan 是不带参数时的默认跨度。
const defaultSeriesSpan = 24 * time.Hour

// SeriesPoint 用 [ts, value] 的二元数组表示，比对象省一半字节。
// unix 秒约 1.8e9，远在 float64 的精确整数范围内，不会有精度问题。
type SeriesPoint [2]float64

// SeriesResponse 是 GET /api/v1/series 的响应体。
type SeriesResponse struct {
	Node   string        `json:"node"`
	Metric string        `json:"metric"`
	Step   int64         `json:"step"`
	From   int64         `json:"from"`
	To     int64         `json:"to"`
	Points []SeriesPoint `json:"points"`
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	nodeID := query.Get("node")
	if _, ok := s.store.Node(nodeID); !ok {
		writeError(w, http.StatusNotFound, protocol.ErrorResponse{
			Code: "unknown_node", Message: "配置里没有这个节点",
		})
		return
	}

	metric := query.Get("metric")
	if metric == "" {
		metric = "cpu_pct"
	}

	now := s.now()
	from, to, err := parseSeriesRange(query, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.ErrorResponse{
			Code: "invalid_field", Field: "from/to", Message: err.Error(),
		})
		return
	}

	// 步长不得细于该节点的原始粒度。
	var minStep int64 = protocol.MinIntervalS
	if interval, ok := s.store.LastInterval(nodeID); ok && int64(interval) > minStep {
		minStep = int64(interval)
	}

	step := store.ChooseStep(int64(to.Sub(from).Seconds()), minStep)
	if raw := query.Get("step"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, protocol.ErrorResponse{
				Code: "invalid_field", Field: "step", Message: "step 必须是正整数（秒）",
			})
			return
		}
		step = parsed
		if step < minStep {
			step = minStep
		}
	}

	points, err := s.store.Series(store.SeriesQuery{
		NodeID: nodeID, Metric: metric, From: from, To: to, Step: step,
	})
	switch {
	case errors.Is(err, store.ErrUnknownMetric):
		writeError(w, http.StatusBadRequest, protocol.ErrorResponse{
			Code:    "invalid_field",
			Field:   "metric",
			Message: "未知指标；可选：" + joinMetrics(),
		})
		return
	case err != nil:
		s.logger.Error("查询曲线失败", "node", nodeID, "metric", metric, "err", err)
		writeError(w, http.StatusInternalServerError, protocol.ErrorResponse{
			Code: "internal_error", Message: "查询失败",
		})
		return
	}

	resp := SeriesResponse{
		Node: nodeID, Metric: metric, Step: step,
		From: from.Unix(), To: to.Unix(),
		// 空结果给空数组而不是 null，前端少一个分支。
		Points: make([]SeriesPoint, 0, len(points)),
	}
	for _, p := range points {
		resp.Points = append(resp.Points, SeriesPoint{float64(p.TS), p.Value})
	}

	// 与 /nodes 同样的理由：数据本身就是 30 秒级的，缓存 15 秒零损失。
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, resp)
}

// parseSeriesRange 解析 from/to（unix 秒），缺省为最近 24 小时。
func parseSeriesRange(query url.Values, now time.Time) (time.Time, time.Time, error) {
	to := now
	if raw := query.Get("to"); raw != "" {
		sec, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to 必须是 unix 秒")
		}
		to = time.Unix(sec, 0)
	}

	from := to.Add(-defaultSeriesSpan)
	if raw := query.Get("from"); raw != "" {
		sec, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from 必须是 unix 秒")
		}
		from = time.Unix(sec, 0)
	}

	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to 必须晚于 from")
	}
	if to.Sub(from) > maxSeriesSpan {
		from = to.Add(-maxSeriesSpan)
	}
	return from, to, nil
}

func joinMetrics() string {
	names := store.Metrics()
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
