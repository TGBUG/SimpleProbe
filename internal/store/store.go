// Package store 是 SQLite 之上的薄封装。
//
// 它同时持有节点身份的内存快照（来自配置）与数据库里的运行状态：因为
// “面板要能显示从未上报过的节点”要求身份与状态来自两处（规格 §2 取舍 1）。
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，不需要 cgo

	"github.com/TGBUG/SimpleProbe/internal/config"
	"github.com/TGBUG/SimpleProbe/internal/protocol"
	"github.com/TGBUG/SimpleProbe/internal/uptime"
)

// schemaStatements 逐条执行：database/sql 的 Exec 不保证支持一次多条语句。
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS node_state (
		node_id       TEXT PRIMARY KEY,
		first_seen    INTEGER NOT NULL,
		last_seen     INTEGER NOT NULL,
		last_interval INTEGER NOT NULL,
		last_state    TEXT    NOT NULL,
		agent_version TEXT    NOT NULL
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS sample (
		node_id   TEXT    NOT NULL,
		ts        INTEGER NOT NULL,
		load1 REAL, load5 REAL, load15 REAL,
		cpu_pct REAL,
		mem_used INTEGER, mem_total INTEGER,
		uptime_s INTEGER,
		PRIMARY KEY (node_id, ts)
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS uptime_bucket (
		node_id TEXT    NOT NULL,
		minute  INTEGER NOT NULL,
		reports INTEGER NOT NULL,
		PRIMARY KEY (node_id, minute)
	) WITHOUT ROWID`,
}

// Store 是并发安全的。
type Store struct {
	db *sql.DB

	mu    sync.RWMutex
	nodes map[string]config.Node
	order []string
}

// Open 打开（必要时创建）数据库，并装入节点身份。
func Open(path string, nodes []config.Node) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=auto_vacuum(INCREMENTAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s: %w", path, err)
	}
	// 连接数固定为 1：写入量极小，单连接彻底避开 SQLITE_BUSY。
	// WAL 在这里的意义是崩溃安全，不是并发。
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.SetNodes(nodes); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	for i, stmt := range schemaStatements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表（第 %d 条）: %w", i+1, err)
		}
	}
	return nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// SetNodes 替换节点身份快照。SIGHUP 热加载走这里，不影响历史数据。
func (s *Store) SetNodes(nodes []config.Node) error {
	index := make(map[string]config.Node, len(nodes))
	order := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if _, dup := index[n.ID]; dup {
			return fmt.Errorf("store: 重复的节点 id %q", n.ID)
		}
		index[n.ID] = n
		order = append(order, n.ID)
	}

	s.mu.Lock()
	s.nodes, s.order = index, order
	s.mu.Unlock()
	return nil
}

// Node 返回节点身份，第二个返回值表示它是否在配置里。
func (s *Store) Node(id string) (config.Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	return n, ok
}

// Record 记录一次成功上报：写明细、累加计数桶、更新当前快照。
//
// at 是 server 的接收时间，是这套系统里唯一的时间权威（规格 §4.1）。
func (s *Store) Record(rep *protocol.Report, at time.Time) error {
	state, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("序列化当前快照: %w", err)
	}
	ts := at.Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	// 提交成功之后 Rollback 是 no-op，所以这里不需要区分成功失败。
	defer func() { _ = tx.Rollback() }()

	// 同一秒内的重复上报（agent 超时重试、但 server 其实已经处理过）直接忽略，
	// 明细里不会出现重复点。
	if _, err := tx.Exec(`
		INSERT INTO sample(node_id, ts, load1, load5, load15, cpu_pct, mem_used, mem_total, uptime_s)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, ts) DO NOTHING`,
		rep.Node, ts,
		rep.Load[0], rep.Load[1], rep.Load[2], rep.CPUPct,
		int64(rep.Mem.Used), int64(rep.Mem.Total), rep.UptimeS,
	); err != nil {
		return fmt.Errorf("写入明细: %w", err)
	}

	// 计数桶按分钟累加。重复上报会让计数偏高，但在线率在上层会被夹到 100%，
	// 所以这里不需要去重——那反而要多一次查询。
	if _, err := tx.Exec(`
		INSERT INTO uptime_bucket(node_id, minute, reports) VALUES(?, ?, 1)
		ON CONFLICT(node_id, minute) DO UPDATE SET reports = reports + 1`,
		rep.Node, ts/60,
	); err != nil {
		return fmt.Errorf("累加计数桶: %w", err)
	}

	// first_seen 只在首次插入时写入，冲突分支刻意不更新它。
	if _, err := tx.Exec(`
		INSERT INTO node_state(node_id, first_seen, last_seen, last_interval, last_state, agent_version)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			last_seen     = excluded.last_seen,
			last_interval = excluded.last_interval,
			last_state    = excluded.last_state,
			agent_version = excluded.agent_version`,
		rep.Node, ts, ts, rep.IntervalS, string(state), rep.AgentVersion,
	); err != nil {
		return fmt.Errorf("更新当前快照: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

// NodeStatus 是面板需要的单个节点状态。
type NodeStatus struct {
	NodeID      string
	DisplayName string
	// Since 是用于裁剪在线率分母的起点：配置声明 > 数据库首次上报 > now。
	Since     time.Time
	FirstSeen time.Time
	LastSeen  time.Time
	IntervalS int
	// Report 是最近一次上报的原文；nil 表示这个节点从未上报过。
	Report *protocol.Report
	Online bool
	// Rates 形如 {"24h": 0.999, "7d": 1}，取值 [0,1]。
	Rates map[string]float64
}

type stateRow struct {
	firstSeen time.Time
	lastSeen  time.Time
	intervalS int
	state     string
	version   string
}

// Snapshot 返回所有配置节点的当前状态。
//
// 返回顺序 = 配置书写顺序；配置里没有、但库里有历史数据的节点不会出现
// （它的身份已经不在了，展示出来也没有名字可用）。
func (s *Store) Snapshot(now time.Time) ([]NodeStatus, error) {
	s.mu.RLock()
	order := append([]string(nil), s.order...)
	nodes := make(map[string]config.Node, len(s.nodes))
	for k, v := range s.nodes {
		nodes[k] = v
	}
	s.mu.RUnlock()

	states, err := s.loadNodeStates()
	if err != nil {
		return nil, err
	}
	counts, err := s.loadReportCounts(now)
	if err != nil {
		return nil, err
	}

	out := make([]NodeStatus, 0, len(order))
	for _, id := range order {
		cfg := nodes[id]
		st := NodeStatus{
			NodeID:      id,
			DisplayName: cfg.DisplayName,
			Rates:       make(map[string]float64, len(uptime.Windows)),
		}

		if row, ok := states[id]; ok {
			st.FirstSeen = row.firstSeen
			st.LastSeen = row.lastSeen
			st.IntervalS = row.intervalS
			var rep protocol.Report
			// 快照解析失败只让当前值缺失，不影响在线率等其它字段。
			if err := json.Unmarshal([]byte(row.state), &rep); err == nil {
				st.Report = &rep
			}
		}

		st.Since = cfg.Since
		if st.Since.IsZero() {
			st.Since = st.FirstSeen
		}
		if st.Since.IsZero() {
			st.Since = now
		}

		var threshold time.Duration
		if st.Report != nil {
			threshold = st.Report.OnlineThreshold()
		}
		st.Online = uptime.Online(st.LastSeen, now, threshold)

		for _, w := range uptime.Windows {
			st.Rates[w.Name] = uptime.Window{
				From:      now.Add(-w.Dur),
				To:        now,
				Since:     st.Since,
				IntervalS: st.IntervalS,
				Reports:   counts[id][w.Name],
			}.Rate()
		}

		out = append(out, st)
	}
	return out, nil
}

func (s *Store) loadNodeStates() (map[string]stateRow, error) {
	rows, err := s.db.Query(
		`SELECT node_id, first_seen, last_seen, last_interval, last_state, agent_version FROM node_state`)
	if err != nil {
		return nil, fmt.Errorf("读取节点状态: %w", err)
	}
	defer rows.Close()

	out := make(map[string]stateRow)
	for rows.Next() {
		var id string
		var r stateRow
		var firstSeen, lastSeen int64
		if err := rows.Scan(&id, &firstSeen, &lastSeen, &r.intervalS, &r.state, &r.version); err != nil {
			return nil, fmt.Errorf("解析节点状态行: %w", err)
		}
		r.firstSeen = time.Unix(firstSeen, 0)
		r.lastSeen = time.Unix(lastSeen, 0)
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节点状态: %w", err)
	}
	return out, nil
}

// loadReportCounts 返回每个节点在各窗口内的上报条数。
//
// 三个窗口各查一次，同一条 SQL 只换时间边界——比一次查 30 天再在内存里分桶
// 更省事，而这套数据量下多扫两次索引的开销可以忽略。
func (s *Store) loadReportCounts(now time.Time) (map[string]map[string]int64, error) {
	out := make(map[string]map[string]int64)

	for _, w := range uptime.Windows {
		rows, err := s.db.Query(
			`SELECT node_id, SUM(reports) FROM uptime_bucket WHERE minute >= ? GROUP BY node_id`,
			now.Add(-w.Dur).Unix()/60)
		if err != nil {
			return nil, fmt.Errorf("统计窗口 %s: %w", w.Name, err)
		}

		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, fmt.Errorf("解析窗口 %s 的统计: %w", w.Name, err)
			}
			if out[id] == nil {
				out[id] = make(map[string]int64, len(uptime.Windows))
			}
			out[id][w.Name] = n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("遍历窗口 %s 的统计: %w", w.Name, err)
		}
		rows.Close()
	}
	return out, nil
}

// Purge 删除过期数据并回收空间。
func (s *Store) Purge(now time.Time, sampleTTL, bucketTTL time.Duration) error {
	if _, err := s.db.Exec(`DELETE FROM sample WHERE ts < ?`, now.Add(-sampleTTL).Unix()); err != nil {
		return fmt.Errorf("清理明细: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM uptime_bucket WHERE minute < ?`, now.Add(-bucketTTL).Unix()/60); err != nil {
		return fmt.Errorf("清理计数桶: %w", err)
	}
	// 只有建库时设了 auto_vacuum=INCREMENTAL，这条才有效。
	if _, err := s.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		return fmt.Errorf("回收空间: %w", err)
	}
	return nil
}
