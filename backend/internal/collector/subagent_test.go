package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentmon/internal/config"
	"agentmon/internal/store"
)

// TestExtractSubagentsDistinctIDs 验证: 同 description 但 tool_use id 不同的两个 Task 调用,
// extractSubagents 应提取出两条独立的子代理记录(各自带不同 ToolUseID)。
// 这是 P3 修复的核心: 之前用 description 做合成 pid 哈希键, 同名 task 会碰撞覆盖。
func TestExtractSubagentsDistinctIDs(t *testing.T) {
	// 两个 tool_use 块: name 都含 "task", description 相同("investigate"), 但 id 不同。
	// 注意: claude transcript 是紧凑 JSONL(每行一个完整 JSON 对象, 内部无换行), 测试数据须与之同构。
	data := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_aaa","name":"Task","input":{"description":"investigate","prompt":"do it"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bbb","name":"Task","input":{"description":"investigate","prompt":"do it"}}]}}`)

	out := extractSubagents(data)
	if len(out) != 2 {
		t.Fatalf("期望 2 条子代理, 实际 %d: %+v", len(out), out)
	}
	if out[0].ToolUseID == out[1].ToolUseID {
		t.Errorf("两个子代理的 ToolUseID 不应相同: %q vs %q", out[0].ToolUseID, out[1].ToolUseID)
	}
	if out[0].ToolUseID != "toolu_aaa" || out[1].ToolUseID != "toolu_bbb" {
		t.Errorf("ToolUseID 提取错误: %q / %q", out[0].ToolUseID, out[1].ToolUseID)
	}
	if out[0].Task != "investigate" || out[1].Task != "investigate" {
		t.Errorf("Task 提取错误: %q / %q", out[0].Task, out[1].Task)
	}
}

// TestMergeTranscriptSubagentsNoCollision 验证: 同一会话文件中, 两个同 description 不同 id 的
// Task 调用, mergeTranscriptSubagents 写入 store 时应生成**不同 pid** 的子代理节点(不再碰撞覆盖)。
func TestMergeTranscriptSubagentsNoCollision(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := &Collector{store: st, cfg: config.Default(), monitors: map[string]*agentMonitor{}, pidOwner: map[int]int{}}

	// 构造一个含两个同 description 不同 id 的 Task 调用的 transcript 文件(紧凑 JSONL)
	dir := t.TempDir()
	tf := filepath.Join(dir, "sess.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_aaa","name":"Task","input":{"description":"investigate","prompt":"do it"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bbb","name":"Task","input":{"description":"investigate","prompt":"do it"}}]}}`
	if err := os.WriteFile(tf, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m := &agentMonitor{}
	ti := transcriptInfo{file: tf, tool: "claude", lastTs: 1}
	active := map[int]bool{}
	c.mergeTranscriptSubagents(m, ti, transcriptPID(tf), time.Now(), active)

	agents, err := st.ListTree()
	if err != nil {
		t.Fatal(err)
	}
	subPids := map[int]bool{}
	for _, a := range agents {
		if a.IsSubagent {
			if subPids[a.PID] {
				t.Errorf("碰撞: 两个子代理 pid 相同 %d", a.PID)
			}
			subPids[a.PID] = true
		}
	}
	if len(subPids) != 2 {
		t.Errorf("期望 2 个不同 pid 的子代理, 实际 %d: %v", len(subPids), subPids)
	}
}
