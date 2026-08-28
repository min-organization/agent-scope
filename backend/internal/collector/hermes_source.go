package collector

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"

	"agentmon/internal/store"
	_ "modernc.org/sqlite"
)

// hermesSource 实现 hermes 的状态推导范式。
// Hermes gateway 是常驻进程，其心跳、SQLite WAL 和网络保活不能视为 agent 工作。
// 优先读取 ~/.hermes/state.db 的会话消息和后台任务状态；数据库不可用时才回退内核态。
type hermesSource struct{}

func (hermesSource) Tool() string { return "hermes" }

// hermes 无 transcript 权威根节点, proc 节点直接展示。
func (hermesSource) HasTranscriptRoot() bool { return false }

func (hermesSource) DeriveProcState(m *agentMonitor, tx *transcriptInfo, idleNs int64) SourceResult {
	if state := readHermesState(idleNs); state.State != "" {
		return state
	}
	stStr, _, needsInput, _, _, _, reason := m.updateState(
		m.idleSeconds, m.waitInputSeconds, m.llmIPs)
	return SourceResult{State: store.AgentState(stStr), Reason: reason, NeedsInput: needsInput}
}

// hermes 无 transcript 节点。
func (hermesSource) DeriveTranscriptState(tx *transcriptInfo, idleNs int64, ptsBlocked bool) SourceResult {
	return SourceResult{}
}

type hermesMessageState struct {
	role         string
	finishReason string
	timestamp    float64
}

func readHermesState(idleNs int64) SourceResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return SourceResult{}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, ".hermes", "state.db")+"?mode=ro")
	if err != nil {
		return SourceResult{}
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	cutoff := float64(time.Now().UnixNano()-idleNs) / float64(time.Second)
	var active int
	err = db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM async_delegations
			WHERE completed_at IS NULL AND updated_at >= ?
			UNION ALL
			SELECT 1 FROM delivery_obligations
			WHERE state NOT IN ('delivered','failed','cancelled') AND updated_at >= ?
		)`, cutoff, cutoff).Scan(&active)
	if err == nil && active != 0 {
		return SourceResult{State: store.StateRunning}
	}

	var msg hermesMessageState
	err = db.QueryRow(`
		SELECT role, COALESCE(finish_reason, ''), timestamp
		FROM messages
		WHERE active = 1 AND role IN ('user','assistant','tool')
		ORDER BY timestamp DESC LIMIT 1`).Scan(&msg.role, &msg.finishReason, &msg.timestamp)
	if err == sql.ErrNoRows {
		return SourceResult{State: store.StateIdle, Reason: store.ReasonIdle}
	}
	if err != nil {
		return SourceResult{}
	}
	return deriveHermesMessageState(msg, cutoff)
}

func deriveHermesMessageState(msg hermesMessageState, cutoff float64) SourceResult {
	if msg.timestamp < cutoff {
		return SourceResult{State: store.StateIdle, Reason: store.ReasonIdle}
	}
	switch msg.role {
	case "user":
		return SourceResult{State: store.StateThinking, Reason: store.ReasonThinkingUser}
	case "tool":
		return SourceResult{State: store.StateThinking, Reason: store.ReasonThinkingLLM}
	case "assistant":
		if msg.finishReason == "tool_calls" {
			return SourceResult{State: store.StateRunning}
		}
		return SourceResult{State: store.StateIdle, Reason: store.ReasonIdle}
	default:
		return SourceResult{}
	}
}
