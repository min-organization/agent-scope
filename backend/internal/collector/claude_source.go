package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agentmon/internal/store"
)

// claudeSource 实现 claude 的状态推导范式。
// claude 主 agent 以 transcript(JSONL) 为权威状态源: 空闲/推理/错误/等待由 transcript 末行推导,
// 避免被内部文件读写误判 running(见 transcriptState)。proc 内核信号(eBPF/pty)作为补充:
//   - transcript 产出 thinking/error 时覆盖 proc 推导(高价值信号)
//   - transcript 产出 waiting(卡在工具批准)时, 仅当 proc 自身不活跃才接受(避免活跃会话误判 waiting)
//   - transcript 产出 idle 时不覆盖 proc 的实时 eBPF/pty 判断
type claudeSource struct{}

func (claudeSource) Tool() string { return "claude" }

// claude 有 transcript 权威根节点, proc root 节点应跳过(由 transcript 节点展示)。
func (claudeSource) HasTranscriptRoot() bool { return true }

func (claudeSource) DeriveProcState(m *agentMonitor, tx *transcriptInfo, idleNs int64) SourceResult {
	// 先取 proc 通用推导(提供 running/thinking/editing/waiting/idle 兜底 + 展示字段)。
	stStr, _, needsInput, _, _, _, _ := m.updateState(
		m.idleSeconds, m.waitInputSeconds, m.llmIPs)
	st := store.AgentState(stStr)
	reason := store.ReasonNone
	errorCode := ""
	// 用转录本覆盖(若可用): 逻辑与历史一致。
	if tx != nil {
		txSt, txReason, txErr := transcriptState(*tx, idleNs)
		switch txSt {
		case "thinking", "error":
			st, reason, errorCode = store.AgentState(txSt), txReason, txErr
			if txSt == "error" {
				needsInput = false
			}
		case "waiting":
			// 仅当 proc 自身非 running/thinking(不活跃)时才接受 waiting,
			// 避免活跃会话(正在处理)被陈旧/卡等判定误显 waiting。
			if st != store.StateRunning && st != store.StateThinking {
				st, reason, needsInput = store.StateWaiting, txReason, true
			}
		}
	}
	return SourceResult{State: st, Reason: reason, ErrorCode: errorCode, NeedsInput: needsInput}
}

// DeriveTranscriptState: claude 会话节点以 transcript 为主，并把 Node.js 的终端事件循环阻塞
// 仅作为组合信号：仍有未完成 tool_use 时表示工具尚未启动、正在等待授权；没有 pending tool
// 时绝不据此把 LLM 推理误报成等待输入。
func (claudeSource) DeriveTranscriptState(tx *transcriptInfo, idleNs int64, inputPollBlocked bool) SourceResult {
	if tx == nil {
		return SourceResult{}
	}
	st, reason, errorCode := transcriptState(*tx, idleNs)
	// Claude 2.1+ 的 live session 文件直接给出 idle/working/waiting 及 waitingFor。
	// 它比 transcript 的未完成工具推断和 /proc 阻塞启发式更准确；错误态仍保持最高优先级。
	if st != "error" && tx.realPID > 0 {
		if live := readClaudeLiveState(tx.realPID); live.State != "" {
			return live
		}
	}
	if st != "error" && inputPollBlocked && tx.pendingToolCount > 0 {
		reason = claudeInteractionReason(tx.pendingToolName)
		if reason == "" {
			reason = store.ReasonAwaitingApproval
		}
		st = "waiting"
	}
	needsInput := st == "waiting"
	return SourceResult{State: store.AgentState(st), Reason: reason, ErrorCode: errorCode, NeedsInput: needsInput}
}

// claudeLiveSession 是 claude 写在 ~/.claude/sessions/<pid>.json 的实时会话记录(节选字段)。
// WaitingFor 仅在 Status=="waiting" 时存在, 描述在等什么(如 "input needed"/"sandbox request")。
type claudeLiveSession struct {
	PID        int    `json:"pid"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
}

func readClaudeLiveState(pid int) SourceResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return SourceResult{}
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return SourceResult{}
	}
	return parseClaudeLiveState(data, pid)
}

// parseClaudeLiveState 把 claude 的实时会话状态映射为本项目状态枚举。
//
// status 取值域固定为 ["busy","shell","idle","waiting"](claude 内部联合类型), 语义:
//
//	busy    isLoading || delegatedActive —— 正在推理 / 子代理运行中(最常见的活跃态)
//	waiting 明确在等用户决策, 同时写 waitingFor 说明等什么
//	idle    非加载中, 等用户下一句
//	shell   用户已切入 shell 交互(非 LLM 活动)
//
// 未知状态一律返回空 SourceResult, 退回 transcript 推导, 不臆造 —— claude 若新增状态,
// 宁可退化到较粗的转录本判定, 也不要把陌生状态硬塞进某个枚举造成误报。
func parseClaudeLiveState(data []byte, expectedPID int) SourceResult {
	var session claudeLiveSession
	if err := json.Unmarshal(data, &session); err != nil || session.PID != expectedPID {
		return SourceResult{}
	}
	switch strings.ToLower(session.Status) {
	case "waiting":
		// waitingFor 实测取值: "input needed" / "sandbox request" / 对话框的等待项。
		// 属"授权确认"语义的归 awaiting_approval, 其余归 awaiting_input。
		reason := store.ReasonAwaitingInput
		waitingFor := strings.ToLower(session.WaitingFor)
		if strings.Contains(waitingFor, "permission") || strings.Contains(waitingFor, "approval") ||
			strings.Contains(waitingFor, "confirm") || strings.Contains(waitingFor, "sandbox") {
			reason = store.ReasonAwaitingApproval
		}
		return SourceResult{State: store.StateWaiting, Reason: reason, NeedsInput: true, Confidence: "high"}
	case "busy":
		return SourceResult{State: store.StateThinking, Reason: store.ReasonThinkingLLM, Confidence: "high"}
	case "idle":
		return SourceResult{State: store.StateIdle, Reason: store.ReasonIdle, Confidence: "high"}
	case "shell":
		// 用户在 shell 交互中: 进程活跃但非 LLM 推理, 且不是"卡住等 agent 授权"。
		// 归 running(不产生 needs_input, 不触发等待类告警)。
		return SourceResult{State: store.StateRunning, Confidence: "high"}
	default:
		return SourceResult{}
	}
}

// codexSource 实现 codex 的状态推导。
// codex 与 claude 同源 transcript(JSONL)格式, 状态推导完全一致, 仅工具名不同。
// 差异化(如未来 codex 特有字段)在此隔离, 不污染 claude 逻辑。
type codexSource struct{}

func (codexSource) Tool() string            { return "codex" }
func (codexSource) HasTranscriptRoot() bool { return true }

func (codexSource) DeriveProcState(m *agentMonitor, tx *transcriptInfo, idleNs int64) SourceResult {
	return claudeSource{}.DeriveProcState(m, tx, idleNs)
}

func (codexSource) DeriveTranscriptState(tx *transcriptInfo, idleNs int64, ptsBlocked bool) SourceResult {
	return claudeSource{}.DeriveTranscriptState(tx, idleNs, ptsBlocked)
}
