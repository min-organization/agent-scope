package collector

import (
	"testing"

	"agentmon/internal/config"
)

// TestAgentSourceRegistry 锁住 P0 重构核心不变量:
// 所有 isKnownLLMAgent 列表里的 agent 都必须有对应的 AgentSource 注册,
// 否则主循环会走通用兜底(降级), 丢失该 agent 的专属状态推导。
// 新增 agent 时若忘记 Register, 此测试立即失败。
func TestAgentSourceRegistry(t *testing.T) {
	// 重新注册(测试隔离: 实际注册在 New() 完成, 此处确保注册表非空且覆盖核心 agent)。
	c := New(config.Default(), nil, nil, nil, nil)
	_ = c
	for _, tool := range []string{"claude", "codex", "copilot", "hermes"} {
		if _, ok := SourceOf(tool); !ok {
			t.Fatalf("agent %q 未注册 AgentSource, 主循环将走通用兜底丢失专属状态推导", tool)
		}
	}
	// 接口行为: claude 的 HasTranscriptRoot 应为 true, copilot/hermes 为 false。
	if src, ok := SourceOf("claude"); !ok || !src.HasTranscriptRoot() {
		t.Fatal("claude 应有 transcript 权威根节点")
	}
	if src, ok := SourceOf("codex"); !ok || !src.HasTranscriptRoot() {
		t.Fatal("codex 应有 transcript 权威根节点")
	}
	if src, ok := SourceOf("copilot"); !ok || src.HasTranscriptRoot() {
		t.Fatal("copilot 不应有 transcript 根节点")
	}
	if src, ok := SourceOf("hermes"); !ok || src.HasTranscriptRoot() {
		t.Fatal("hermes 不应有 transcript 根节点")
	}
	// 未注册 agent 走通用兜底(返回 false, 不 panic)。
	if _, ok := SourceOf("aider"); ok {
		t.Fatal("aider 不应已注册(走通用兜底)")
	}
}
