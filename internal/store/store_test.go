package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/TGBUG/SimpleProbe/internal/config"
	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/uptime"
)

func newStore(t *testing.T, nodes ...config.Node) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.db")
	s, err := Open(path, nodes)
	if err != nil {
		t.Fatalf("Open() 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testNode(id string) config.Node {
	return config.Node{ID: id, DisplayName: "节点 " + id, Token: "token-" + id}
}

func testReport(node string, intervalS int) *protocol.Report {
	return &protocol.Report{
		V: 1, Node: node, IntervalS: intervalS,
		Load:    []float64{1, 2, 3},
		CPUPct:  12.5,
		Mem:     protocol.Mem{Used: 100, Total: 200},
		Disk:    []protocol.Disk{{Mount: "/", Used: 1, Total: 2}},
		UptimeS: 42, AgentVersion: "0.1.0",
	}
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询 %q 失败: %v", query, err)
	}
	return n
}

// TestSnapshot_NeverReportedNode 钉住规格 §2 取舍 1 的那个性质：
// 配置里声明了、但从未上报的节点，必须出现在面板上并显示“从未上线”。
func TestSnapshot_NeverReportedNode(t *testing.T) {
	s := newStore(t, testNode("web01"))
	now := time.Unix(1_700_000_000, 0)

	got, err := s.Snapshot(now)
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("节点数 = %d，期望 1", len(got))
	}

	st := got[0]
	if st.NodeID != "web01" || st.DisplayName != "节点 web01" {
		t.Errorf("身份不对: %+v", st)
	}
	if st.Report != nil {
		t.Error("从未上报的节点，Report 应为 nil")
	}
	if st.Online {
		t.Error("从未上报的节点不应显示为在线")
	}
	for _, w := range uptime.Windows {
		if st.Rates[w.Name] != 0 {
			t.Errorf("窗口 %s 的在线率 = %v，期望 0", w.Name, st.Rates[w.Name])
		}
	}
	if !st.Since.Equal(now) {
		t.Errorf("从未上报时 Since 应回落到 now，得到 %v", st.Since)
	}
}

func TestRecord_ThenSnapshot(t *testing.T) {
	s := newStore(t, testNode("web01"))
	at := time.Unix(1_700_000_000, 0)

	if err := s.Record(testReport("web01", 30), at); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}

	got, err := s.Snapshot(at.Add(5 * time.Second))
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	st := got[0]

	if st.Report == nil {
		t.Fatal("Report 不应为 nil")
	}
	if st.Report.CPUPct != 12.5 || st.Report.UptimeS != 42 || st.Report.AgentVersion != "0.1.0" {
		t.Errorf("快照内容不对: %+v", st.Report)
	}
	if len(st.Report.Disk) != 1 || st.Report.Disk[0].Mount != "/" {
		t.Errorf("磁盘字段丢失: %+v", st.Report.Disk)
	}
	if !st.Online {
		t.Error("5 秒前刚上报过，应显示在线（阈值 75 秒）")
	}
	if !st.FirstSeen.Equal(at) || !st.LastSeen.Equal(at) {
		t.Errorf("first_seen/last_seen = %v/%v，期望 %v", st.FirstSeen, st.LastSeen, at)
	}
	if st.IntervalS != 30 {
		t.Errorf("IntervalS = %d，期望 30", st.IntervalS)
	}
}

func TestSnapshot_OnlineGoesFalseAfterThreshold(t *testing.T) {
	s := newStore(t, testNode("web01"))
	at := time.Unix(1_700_000_000, 0)
	if err := s.Record(testReport("web01", 30), at); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}

	got, err := s.Snapshot(at.Add(76 * time.Second)) // 阈值 75 秒
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	if got[0].Online {
		t.Error("超过 2.5 × 30 秒后应判定为离线")
	}
}

// TestOnlineRate_RespectsSince 是规格 §4.2 那个公式的端到端验证：
// 一小时窗口内按 30 秒间隔应收到 120 条，实际只给 60 条 → 在线率 0.5。
func TestOnlineRate_RespectsSince(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	node := testNode("web01")
	node.Since = now.Add(-1 * time.Hour)
	s := newStore(t, node)

	// 每分钟一条，共 60 条，落在 60 个不同的分钟桶里。
	for i := 0; i < 60; i++ {
		at := now.Add(-time.Hour + time.Duration(i)*time.Minute)
		if err := s.Record(testReport("web01", 30), at); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}

	got, err := s.Snapshot(now)
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	if r := got[0].Rates["24h"]; math.Abs(r-0.5) > 1e-9 {
		t.Errorf("24h 在线率 = %v，期望 0.5（分母被 since 裁到 1 小时 = 120 条期望）", r)
	}
}

// TestOnlineRate_WithoutSinceClampWouldBeWrong 是上面那条的反面：
// 如果分母按完整 24 小时算，同样的数据会得到 0.02 左右而不是 0.5。
func TestOnlineRate_WithoutSinceClampWouldBeWrong(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := uptime.Window{
		From: now.Add(-24 * time.Hour), To: now,
		Since:     now.Add(-24 * time.Hour), // 故意不裁剪
		IntervalS: 30, Reports: 60,
	}
	unclamped := w.Rate()
	if !(unclamped > 0.02 && unclamped < 0.03) {
		t.Fatalf("未裁剪的在线率 = %v，期望约 0.0208", unclamped)
	}
}

// TestRecord_DuplicateSameSecond 钉住两个刻意的取舍：
// 明细按 (node_id, ts) 去重，计数桶却会累加——后者靠上层夹紧兜住。
func TestRecord_DuplicateSameSecond(t *testing.T) {
	s := newStore(t, testNode("web01"))
	at := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		if err := s.Record(testReport("web01", 30), at); err != nil {
			t.Fatalf("Record() 失败: %v", err)
		}
	}

	if n := count(t, s, `SELECT COUNT(*) FROM sample`); n != 1 {
		t.Errorf("明细行数 = %d，期望 1（同一秒只留一个点）", n)
	}

	var reports int
	if err := s.db.QueryRow(`SELECT reports FROM uptime_bucket`).Scan(&reports); err != nil {
		t.Fatalf("读取计数桶失败: %v", err)
	}
	if reports != 3 {
		t.Errorf("计数桶 = %d，期望 3（重复上报会累加，由上层夹紧到 100%%）", reports)
	}
}

func TestPurge(t *testing.T) {
	s := newStore(t, testNode("web01"))
	now := time.Unix(1_700_000_000, 0)

	if err := s.Record(testReport("web01", 30), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}
	if err := s.Record(testReport("web01", 30), now); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}

	if err := s.Purge(now, 24*time.Hour, 365*24*time.Hour); err != nil {
		t.Fatalf("Purge() 失败: %v", err)
	}

	if n := count(t, s, `SELECT COUNT(*) FROM sample`); n != 1 {
		t.Errorf("清理后明细行数 = %d，期望 1", n)
	}
	// 计数桶保留期更长，两个桶都还在。
	if n := count(t, s, `SELECT COUNT(*) FROM uptime_bucket`); n != 2 {
		t.Errorf("计数桶行数 = %d，期望 2（保留期是 1 年）", n)
	}

	// 把桶的保留期缩短到 1 小时：48 小时前那个桶该被删掉，
	// 而 now 这一分钟的桶落在保留期内，必须留下。
	if err := s.Purge(now, 24*time.Hour, time.Hour); err != nil {
		t.Fatalf("Purge() 失败: %v", err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM uptime_bucket`); n != 1 {
		t.Errorf("清理后计数桶行数 = %d，期望 1（只剩最近那一分钟的桶）", n)
	}
}

func TestSnapshot_FollowsConfigOrder(t *testing.T) {
	s := newStore(t, testNode("zebra"), testNode("alpha"), testNode("mid"))
	got, err := s.Snapshot(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	want := []string{"zebra", "alpha", "mid"}
	for i, id := range want {
		if got[i].NodeID != id {
			t.Errorf("第 %d 个节点 = %q，期望 %q（顺序应跟配置书写顺序）", i, got[i].NodeID, id)
		}
	}
}

func TestSetNodes_RejectsDuplicate(t *testing.T) {
	s := newStore(t, testNode("a"))
	if err := s.SetNodes([]config.Node{testNode("a"), testNode("a")}); err == nil {
		t.Fatal("重复 id 应报错")
	}
}

func TestSetNodes_HotReloadKeepsHistory(t *testing.T) {
	s := newStore(t, testNode("web01"))
	at := time.Unix(1_700_000_000, 0)
	if err := s.Record(testReport("web01", 30), at); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}

	// 热加载：加入一个新节点、保留原来的。
	if err := s.SetNodes([]config.Node{testNode("web01"), testNode("nas")}); err != nil {
		t.Fatalf("SetNodes() 失败: %v", err)
	}

	got, err := s.Snapshot(at)
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("节点数 = %d，期望 2", len(got))
	}
	if got[0].Report == nil {
		t.Error("热加载后原有节点的历史状态不应丢失")
	}
	if got[1].Report != nil {
		t.Error("新加入的节点应为从未上报")
	}
}

func TestNode_Lookup(t *testing.T) {
	s := newStore(t, testNode("web01"))

	n, ok := s.Node("web01")
	if !ok || n.Token != "token-web01" {
		t.Errorf("Node(web01) = %+v, %v", n, ok)
	}
	if _, ok := s.Node("不存在"); ok {
		t.Error("不存在的节点不应返回 ok")
	}
}

func TestPersistence_Reopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	at := time.Unix(1_700_000_000, 0)

	s1, err := Open(path, []config.Node{testNode("web01")})
	if err != nil {
		t.Fatalf("Open() 失败: %v", err)
	}
	if err := s1.Record(testReport("web01", 30), at); err != nil {
		t.Fatalf("Record() 失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close() 失败: %v", err)
	}

	s2, err := Open(path, []config.Node{testNode("web01")})
	if err != nil {
		t.Fatalf("重新 Open() 失败: %v", err)
	}
	defer s2.Close()

	got, err := s2.Snapshot(at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Snapshot() 失败: %v", err)
	}
	if got[0].Report == nil || got[0].Report.UptimeS != 42 {
		t.Errorf("重开后数据丢失: %+v", got[0].Report)
	}
}
