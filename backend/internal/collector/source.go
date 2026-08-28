package collector

import (
	"strings"
	"time"

	"agentmon/internal/store"
)

// 注: 状态枚举定义于 store 包(数据模型的单一真相), 本包复用 store.AgentState。
// 取值: running / thinking / editing / waiting / idle / error(见 store.State* 常量)。

// SourceResult 是 AgentSource 推导状态的统一返回结构。
type SourceResult struct {
	State      store.AgentState // 状态(空字符串表示"本 Source 不覆盖, 用通用推导")
	Reason     string           // 状态补充原因枚举(写入 state_reason, 可本地化, 见 store.Reason*)
	ErrorCode  string           // llm_error 时的错误码(如 "429"); 其他状态空
	NeedsInput bool             // 是否阻塞在等用户输入
	Task       string           // 给前端 Task 列的摘要(空则主循环用通用 taskOf 计算)
	Confidence string           // 可选置信度覆盖(high/medium/low)
}

// AgentSource 定义某类 agent 的状态推导范式。
// 每个 agent(claude/copilot/hermes/codex...) 实现自己的 Source 并注册到 registry,
// 主循环只通过接口交互, 不在 collector.go 写 if tool== 硬分支(开闭原则)。
type AgentSource interface {
	// Tool 返回该 Source 负责的 agent 名(与 matchTool 命中名一致, 小写)。
	Tool() string

	// HasTranscriptRoot 表示该 agent 是否有"以转录本为权威根节点"的会话
	// (claude/codex 的 transcript JSONL 是状态权威源, proc root 节点应跳过由 transcript 节点展示;
	//  copilot/hermes 无转录本根节点, proc 节点直接展示)。
	HasTranscriptRoot() bool

	// DeriveProcState 推导 proc 真实进程节点状态。
	// m 提供 eBPF/pty 等内核态信号(通用); tx 为该 agent 的转录本(可能为 nil, 无转录本时)。
	// idleNs 为超时阈值(纳秒)。
	DeriveProcState(m *agentMonitor, tx *transcriptInfo, idleNs int64) SourceResult

	// DeriveTranscriptState 推导 transcript 会话节点状态。ptsBlocked 是可选的只读内核提示，
	// 但实现不得仅凭该模糊信号把推理或工具执行升级为等待输入。
	DeriveTranscriptState(tx *transcriptInfo, idleNs int64, ptsBlocked bool) SourceResult
}

// registry 按 tool 名索引已注册的 AgentSource。主循环通过 SourceOf 查表, 无硬分支。
var registry = map[string]AgentSource{}

// Register 注册一个 AgentSource(通常在 New 中调用)。
func Register(s AgentSource) {
	registry[strings.ToLower(s.Tool())] = s
}

// SourceOf 按 tool 名查 Source。未注册(返回 false)时主循环走通用 updateState 兜底,
// 兼容 aider/gemini 等尚未精细实现的 agent。
func SourceOf(tool string) (AgentSource, bool) {
	s, ok := registry[strings.ToLower(tool)]
	return s, ok
}

// knownAgentTools 返回所有已注册的 agent 工具名(用于测试/校验注册表完整性)。
func knownAgentTools() []string {
	tools := make([]string, 0, len(registry))
	for t := range registry {
		tools = append(tools, t)
	}
	return tools
}

// timeNowNs 统一时间源(便于测试注入, 目前直连 time.Now)。
func timeNowNs() int64 { return time.Now().UnixNano() }
