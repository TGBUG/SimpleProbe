package store

import (
	"math"
	"testing"
	"time"
)

// TestOnlineRate_NinetySecondInterval 是规格 §4.2 那个反例的端到端版本。
//
// 节点配成 90 秒上报、全程在线，在线率必须是 100%。如果谁把算法改回
// “有数据的分钟数 / 总分钟数”，这里会得到约 0.67——因为 90 秒的间隔
// 注定有三分之一的自然分钟里没有数据。
func TestOnlineRate_NinetySecondInterval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	node := testNode("slow")
	node.Since = now.Add(-time.Hour)
	s := newStore(t, node)

	// 一小时、每 90 秒一条 = 40 条，正好等于 3600/90。
	for i := 0; i < 40; i++ {
		at := now.Add(-time.Hour + time.Duration(i)*90*time.Second)
		if err := s.Record(testReport("slow", 90), at); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}

	got, err := s.Snapshot(now)
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}

	rate := got[0].Rates["24h"]
	if math.Abs(rate-1) > 1e-9 {
		t.Errorf("90 秒间隔、全程在线的在线率 = %v，期望 1"+
			"（若按分钟桶算会得到约 0.67，说明算法退化了）", rate)
	}
}

// TestOnlineRate_MatchesCountNotBuckets 用同一份数据给出两种算法的对照，
// 把“为什么必须是计数法”钉在测试里。
func TestOnlineRate_MatchesCountNotBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	node := testNode("slow")
	node.Since = now.Add(-time.Hour)
	s := newStore(t, node)

	const reports = 40
	for i := 0; i < reports; i++ {
		at := now.Add(-time.Hour + time.Duration(i)*90*time.Second)
		if err := s.Record(testReport("slow", 90), at); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}

	got, err := s.Snapshot(now)
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	countBased := got[0].Rates["24h"]

	// 反事实：按“有数据的分钟数 / 60 分钟”来算。
	var minutesWithData int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT minute) FROM uptime_bucket`).Scan(&minutesWithData); err != nil {
		t.Fatalf("统计分钟桶失败: %v", err)
	}
	bucketBased := float64(minutesWithData) / 60

	if math.Abs(countBased-1) > 1e-9 {
		t.Errorf("计数法在线率 = %v，期望 1", countBased)
	}
	if !(bucketBased > 0.6 && bucketBased < 0.7) {
		t.Fatalf("分钟桶法在线率 = %v，期望约 0.67（这个反事实是测试的前提）", bucketBased)
	}
	if countBased <= bucketBased {
		t.Errorf("计数法(%v) 应高于分钟桶法(%v)", countBased, bucketBased)
	}
}
