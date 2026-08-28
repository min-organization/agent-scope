package collector

import "agentmon/internal/store"

// copilotSource 实现 copilot 的状态推导范式。
// copilot 以自身写入的 events.jsonl 为权威状态源(解决了"用户随时可输入" vs
// "需要用户输入才能继续" 的内核态无法区分难题: copilot 在 ask_user / permission.requested
// 时明确写进 events.jsonl)。因此 copilot 完全信任 parseCopilotState 的返回, 不走 proc 内核推断。
// 置信度由主循环统一设为 high(状态来自 agent 自身权威事件, 非内核推断)。
type copilotSource struct{}

func (copilotSource) Tool() string { return "copilot" }

// copilot 无 transcript 权威根节点, proc 节点直接展示。
func (copilotSource) HasTranscriptRoot() bool { return false }

func (copilotSource) DeriveProcState(m *agentMonitor, tx *transcriptInfo, idleNs int64) SourceResult {
	state := parseCopilotStateResultForPID(m.pid)
	if state.state == "" {
		return SourceResult{} // 无 copilot 会话时, 不覆盖(走通用 updateState)
	}
	// task 已含 copilot 专属(工具名/文件)中性摘要, 直接作为 Task 列(数据非 UI 文案, 无需本地化)。
	// reason 由状态枚举推导, 供前端 i18n 渲染(等待授权/推理中等语义)。
	return SourceResult{
		State:      store.AgentState(state.state),
		Reason:     state.reason,
		NeedsInput: state.needsInput,
		Task:       state.task,
	}
}

// copilot 无 transcript 节点。
func (copilotSource) DeriveTranscriptState(tx *transcriptInfo, idleNs int64, ptsBlocked bool) SourceResult {
	return SourceResult{}
}
