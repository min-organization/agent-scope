package collector

import (
	"testing"

	"agentmon/internal/store"
)

func TestDeriveHermesMessageState(t *testing.T) {
	const cutoff = 100
	tests := []struct {
		name string
		msg  hermesMessageState
		want store.AgentState
		rsn  string
	}{
		{"stale", hermesMessageState{role: "user", timestamp: 99}, store.StateIdle, store.ReasonIdle},
		{"user", hermesMessageState{role: "user", timestamp: 101}, store.StateThinking, store.ReasonThinkingUser},
		{"tool result", hermesMessageState{role: "tool", timestamp: 101}, store.StateThinking, store.ReasonThinkingLLM},
		{"tool call", hermesMessageState{role: "assistant", finishReason: "tool_calls", timestamp: 101}, store.StateRunning, ""},
		{"final answer", hermesMessageState{role: "assistant", finishReason: "stop", timestamp: 101}, store.StateIdle, store.ReasonIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveHermesMessageState(tt.msg, cutoff)
			if got.State != tt.want || got.Reason != tt.rsn {
				t.Fatalf("got state=%q reason=%q, want %q/%q", got.State, got.Reason, tt.want, tt.rsn)
			}
		})
	}
}
