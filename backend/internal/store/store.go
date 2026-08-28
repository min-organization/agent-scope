package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// AgentState 是 agent 状态的强类型枚举(单一真相, 前后端契约)。
// 取值: running / thinking / editing / waiting / idle / error。
// 历史上曾有 blocked/unknown/done, 已在状态审计中删除, 不再保留(干净断代)。
type AgentState string

const (
	StateRunning  AgentState = "running"
	StateThinking AgentState = "thinking"
	StateEditing  AgentState = "editing"
	StateWaiting  AgentState = "waiting"
	StateIdle     AgentState = "idle"
	StateError    AgentState = "error"
)

// Valid 校验状态取值合法(契约校验, 防止后端产出未知状态)。
func (s AgentState) Valid() bool {
	switch s {
	case StateRunning, StateThinking, StateEditing, StateWaiting, StateIdle, StateError:
		return true
	}
	return false
}

// StateReason 是状态补充原因的强类型枚举(单一真相, 前后端契约, 可本地化)。
// 与 State 不同: State 是粗粒度状态机状态, Reason 是更精确的原因细分,
// 供前端 i18n 渲染(避免后端硬编码中文导致与 locale 混排)。
// 空字符串("")表示无额外原因说明(仅靠 State label 即可表达)。
const (
	ReasonNone             = ""                  // 无额外原因
	ReasonAwaitingApproval = "awaiting_approval" // 等待用户授权 / 确认(tool_use 批准等)
	ReasonAwaitingInput    = "awaiting_input"    // 等待用户输入(claude 阻塞在 tty 读取, 等键盘; 无 tool_use 结构化信号时的纯 CLI 交互等待)
	ReasonLLMError         = "llm_error"         // LLM 接口错误(错误码见 StateErrorCode)
	ReasonThinkingLLM      = "thinking_llm"      // 调用 LLM / 推理中
	ReasonThinkingUser     = "thinking_user"     // 处理用户输入中(新消息到达后)
	ReasonIdle             = "idle"              // 空闲 / 无活动
)

// Agent 单个被观测进程/会话节点(扁平存储, 树关系由 parent_pid/children 表达)。
type Agent struct {
	PID        int        `json:"pid"`
	Tool       string     `json:"tool"`
	State      AgentState `json:"state"`
	Confidence string     `json:"confidence"` // high / medium / low
	LastText   string     `json:"last_text"`
	UpdatedAt  int64      `json:"updated_at"`
	// 行为采集新增字段
	LastCmd        string `json:"last_cmd"`         // 最近执行的命令 basename
	LastFile       string `json:"last_file"`        // 最近打开/编辑的文件 basename
	LastConn       string `json:"last_conn"`        // 最近网络连接 host:port
	StateReason    string `json:"state_reason"`     // 状态补充原因枚举(可本地化, 见 store.Reason*)
	StateErrorCode string `json:"state_error_code"` // llm_error 时的错误码(如 "429"), 其他状态空
	NeedsInput     bool   `json:"needs_input"`      // 阻塞在 pty 输入, 等待用户操作
	// 进程树字段(彻底重构: 父子 agent 关系)
	ParentPID  int    `json:"parent_pid"`  // 父节点 pid(根节点为 0)
	RootPID    int    `json:"root_pid"`    // 所属根 agent pid(自身为根时为自身 pid)
	Depth      int    `json:"depth"`       // 树深度(根=0)
	IsSubagent bool   `json:"is_subagent"` // 是否为子 agent(进程子节点 或 同会话子代理)
	Task       string `json:"task"`        // 该节点正在执行的任务描述(子代理名/工具/文件)
	Src        string `json:"src"`         // 来源: proc(进程) / transcript(同会话子代理) / mixed
	Children   []int  `json:"children"`    // 子节点 pid 列表(从 parent_pid 反查组装)
}

// Event 行为时间线事件。仅元数据, 无隐私风险。
type Event struct {
	PID      int        `json:"pid"`
	Ts       int64      `json:"ts"`
	Kind     string     `json:"kind"`                // cmd / edit / conn / state
	Detail   string     `json:"detail"`              // 命令名/文件名/host:port/状态
	State    AgentState `json:"state"`               // kind=state 时的状态值
	FileKind string     `json:"file_kind,omitempty"` // kind=edit 时: user(业务文件) / agent_temp(agent 内部临时文件)
}

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

// ensureAgentsSchema 创建 agents 表(含进程树新列); 若表已存在但缺新列(旧版 schema),
// 逐列 ALTER TABLE ADD COLUMN 补列(避免 DROP TABLE 导致数据丢失及并发竞态)。
func ensureAgentsSchema(db *sql.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS agents (
		pid INTEGER PRIMARY KEY,
		tool TEXT,
		state TEXT,
		confidence TEXT,
		last_text TEXT,
		updated_at INTEGER,
		last_cmd TEXT,
		last_file TEXT,
		last_conn TEXT,
		state_reason TEXT NOT NULL DEFAULT '',
		state_error_code TEXT NOT NULL DEFAULT '',
		needs_input INTEGER DEFAULT 0,
		parent_pid INTEGER DEFAULT 0,
		root_pid INTEGER DEFAULT 0,
		depth INTEGER DEFAULT 0,
		is_subagent INTEGER DEFAULT 0,
		task TEXT DEFAULT '',
		src TEXT DEFAULT 'proc'
	)`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("建表: %w", err)
	}

	// 新列清单: 逐列用 ALTER TABLE ADD COLUMN 补(避免 DROP TABLE 数据丢失及并发竞态)。
	// 若列已存在 SQLite 返回"duplicate column name"错误, 忽略它。
	newColumns := []struct {
		name string
		typ  string
		dflt string
	}{
		{"parent_pid", "INTEGER", "0"},
		{"root_pid", "INTEGER", "0"},
		{"depth", "INTEGER", "0"},
		{"is_subagent", "INTEGER", "0"},
		{"task", "TEXT", "''"},
		{"src", "TEXT", "'proc'"},
		{"state_reason", "TEXT NOT NULL", "''"},
		{"state_error_code", "TEXT NOT NULL", "''"},
	}
	for _, c := range newColumns {
		_, err := db.Exec(fmt.Sprintf(`ALTER TABLE agents ADD COLUMN %s %s DEFAULT %s`, c.name, c.typ, c.dflt))
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("补列 %s: %w", c.name, err)
		}
	}
	return nil
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // 纯 Go sqlite 单连接避免锁
	// 进程树重构: agents 表新增 parent_pid/root_pid/depth/is_subagent/task/src 列。
	// 旧版(v1.7-) schema 无这些列, 直接 CREATE TABLE IF NOT EXISTS 不会补列 -> 后续索引/读写报错。
	// 补列(ALTER TABLE ADD COLUMN), 不丢失数据
	if err := ensureAgentsSchema(db); err != nil {
		return nil, fmt.Errorf("agents 表 schema: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_agents_root ON agents(root_pid)`); err != nil {
		return nil, fmt.Errorf("建索引: %w", err)
	}
	st := &Store{mu: sync.Mutex{}, db: db}
	if err := st.initEvents(); err != nil {
		return nil, fmt.Errorf("建事件表: %w", err)
	}
	if err := st.initAlerts(); err != nil {
		return nil, fmt.Errorf("建告警表: %w", err)
	}
	return st, nil
}

func (s *Store) Upsert(a Agent) error {
	if !a.State.Valid() {
		return fmt.Errorf("invalid agent state %q", a.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO agents (pid, tool, state, confidence, last_text, updated_at, last_cmd, last_file, last_conn, state_reason, state_error_code, needs_input, parent_pid, root_pid, depth, is_subagent, task, src)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(pid) DO UPDATE SET
		   tool=excluded.tool, state=excluded.state, confidence=excluded.confidence,
		   last_text=excluded.last_text, updated_at=excluded.updated_at,
		   last_cmd=excluded.last_cmd, last_file=excluded.last_file,
		   last_conn=excluded.last_conn, state_reason=excluded.state_reason, state_error_code=excluded.state_error_code,
		   needs_input=excluded.needs_input, parent_pid=excluded.parent_pid,
		   root_pid=excluded.root_pid, depth=excluded.depth, is_subagent=excluded.is_subagent,
		   task=excluded.task, src=excluded.src`,
		a.PID, a.Tool, a.State, a.Confidence, a.LastText, a.UpdatedAt,
		a.LastCmd, a.LastFile, a.LastConn, a.StateReason, a.StateErrorCode, boolToInt(a.NeedsInput),
		a.ParentPID, a.RootPID, a.Depth, boolToInt(a.IsSubagent), a.Task, a.Src,
	)
	return err
}

// Prune 删除 updated_at 早于 before 的记录(进程已消失)
func (s *Store) Prune(before int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM agents WHERE updated_at < ?`, before)
	return err
}

// List 返回全部节点(扁平)。
func (s *Store) List() ([]Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT pid, tool, state, confidence, last_text, updated_at, last_cmd, last_file, last_conn, state_reason, state_error_code, needs_input, parent_pid, root_pid, depth, is_subagent, task, src FROM agents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var needsInput, isSub int
		if err := rows.Scan(&a.PID, &a.Tool, &a.State, &a.Confidence, &a.LastText, &a.UpdatedAt,
			&a.LastCmd, &a.LastFile, &a.LastConn, &a.StateReason, &a.StateErrorCode, &needsInput,
			&a.ParentPID, &a.RootPID, &a.Depth, &isSub, &a.Task, &a.Src); err != nil {
			return nil, err
		}
		a.NeedsInput = needsInput != 0
		a.IsSubagent = isSub != 0
		out = append(out, a)
	}
	return out, nil
}

// ListTree 返回全部 agent 节点(扁平列表), 每个节点的 Children 填为其直接子 pid 数组(仅存在的节点)。
// 前端/客户端按 parent_pid + byPid 自行组装成树(便于递归渲染与筛选)。
func (s *Store) ListTree() ([]Agent, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	exists := make(map[int]bool, len(all))
	for _, a := range all {
		exists[a.PID] = true
	}
	childrenOf := make(map[int][]int)
	for _, a := range all {
		if a.ParentPID != 0 && exists[a.ParentPID] {
			childrenOf[a.ParentPID] = append(childrenOf[a.ParentPID], a.PID)
		}
	}
	for i := range all {
		all[i].Children = childrenOf[all[i].PID]
	}
	return all, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) Close() error {
	return s.db.Close()
}

// ---- 异常告警 alerts ----

func (s *Store) initAlerts() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pid INTEGER,
		tool TEXT,
		kind TEXT,
		level TEXT,
		message TEXT,
		ts INTEGER
	)`)
	return err
}

func (s *Store) RecordAlert(a AlertRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO alerts (pid, tool, kind, level, message, ts) VALUES (?,?,?,?,?,?)`,
		a.PID, a.Tool, a.Kind, a.Level, a.Message, a.TS,
	)
	return err
}

// AlertOut 异常告警对外结构(供前端展示)。
type AlertOut struct {
	ID      int64  `json:"id"`
	PID     int    `json:"pid"`
	Tool    string `json:"tool"`
	Kind    string `json:"kind"`
	Level   string `json:"level"`
	Message string `json:"message"`
	TS      int64  `json:"ts"`
}

func (s *Store) RecentAlerts(limit int) ([]AlertOut, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, pid, tool, kind, level, message, ts FROM alerts ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertOut
	for rows.Next() {
		var a AlertOut
		if err := rows.Scan(&a.ID, &a.PID, &a.Tool, &a.Kind, &a.Level, &a.Message, &a.TS); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// AlertRecord 异常告警记录。
type AlertRecord struct {
	PID     int
	Tool    string
	Kind    string
	Level   string
	Message string
	TS      int64
}

// ---- 行为时间线 events ----

func (s *Store) initEvents() error {
	const ddl = `CREATE TABLE IF NOT EXISTS events (
		pid INTEGER,
		ts INTEGER,
		kind TEXT,
		detail TEXT,
		state TEXT,
		file_kind TEXT DEFAULT '',
		idx INTEGER PRIMARY KEY AUTOINCREMENT
	)`
	// 旧表可能无 file_kind 列 -> 补列(不清数据)
	if _, err := s.db.Exec(ddl); err != nil {
		return err
	}
	var hasFK string
	if err := s.db.QueryRow(`SELECT name FROM pragma_table_info('events') WHERE name='file_kind'`).Scan(&hasFK); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("查 file_kind 列: %w", err)
	}
	if hasFK == "" {
		if _, err := s.db.Exec(`ALTER TABLE events ADD COLUMN file_kind TEXT DEFAULT ''`); err != nil {
			return fmt.Errorf("补 file_kind 列: %w", err)
		}
	}
	return nil
}

// RecordEvent 写入一条行为事件
func (s *Store) RecordEvent(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO events (pid, ts, kind, detail, state, file_kind) VALUES (?,?,?,?,?,?)`,
		e.PID, e.Ts, e.Kind, e.Detail, e.State, e.FileKind,
	)
	return err
}

// RecentEvents 返回某 pid 最近 limit 条事件(按时间倒序)。
// onlyUser=true 时仅返回 file_kind='user' 的 edit 事件(默认隐藏 agent 内部临时文件)。
func (s *Store) RecentEvents(pid, limit int, onlyUser bool) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT pid, ts, kind, detail, state, file_kind FROM events WHERE pid=?`
	if onlyUser {
		q += ` AND (kind != 'edit' OR file_kind = 'user')`
	}
	q += ` ORDER BY idx DESC LIMIT ?`
	rows, err := s.db.Query(q, pid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.PID, &e.Ts, &e.Kind, &e.Detail, &e.State, &e.FileKind); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	// 反转成时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// PruneEvents 删除早于 before 的所有事件(进程消失后清理)
func (s *Store) PruneEvents(before int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, before)
	return err
}

// PruneAlerts 删除早于 before 的所有告警(防止 alerts 表无限增长)。
// before 由调用方按 Store.RetainAlertsDays 计算; 若 retain=0 则不调用本函数(永久保留)。
func (s *Store) PruneAlerts(before int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM alerts WHERE ts < ?`, before)
	return err
}

// DeleteAlertsKind 删除某 pid 指定 kind 的告警(用于状态告警自动解除: 触发条件消失即清掉陈旧记录)。
func (s *Store) DeleteAlertsKind(pid int, kind string) error {
	if s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM alerts WHERE pid=? AND kind=?`, pid, kind)
	return err
}

// DeleteOrphanStateAlerts 批量删除"状态型"孤儿告警:
// 这些告警(llm_error/stuck/wait_unhandled)绑定 agent 生命周期, 当 pid 不在 active 集合中
// (agent 已退出/会话已归档)时自动清除, 避免告警残留到 retention 到期。
// 安全审计类(secret_leak/destructive_cmd)保留以追溯, 不受影响。
func (s *Store) DeleteOrphanStateAlerts(activePIDs []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(activePIDs) == 0 {
		// 无活跃 agent, 清理全部状态告警
		_, err := s.db.Exec(`DELETE FROM alerts WHERE kind IN ('llm_error','stuck','wait_unhandled')`)
		return err
	}
	// 用 IN 子句: WHERE pid NOT IN (?) AND kind IN (...)
	// SQLite 不支持参数化 IN, 用 ? 占位拼接
	qs := make([]string, len(activePIDs))
	args := make([]interface{}, 0, len(activePIDs)+3)
	for i, pid := range activePIDs {
		qs[i] = "?"
		args = append(args, pid)
	}
	args = append(args, "llm_error", "stuck", "wait_unhandled")
	q := fmt.Sprintf(`DELETE FROM alerts WHERE pid NOT IN (%s) AND kind IN (?,?,?)`, strings.Join(qs, ","))
	_, err := s.db.Exec(q, args...)
	return err
}

var _ = time.Now
var _ = json.Marshal
var _ = strings.Contains
