package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentmon/internal/config"
	"agentmon/internal/store"
)

// 真实形态的 Claude Code transcript 片段: 一个 assistant 消息内含两个 Task 子代理调用。
// 依据官方 transcript 结构: 每行一个 JSON, message.content 为块数组, tool_use 块含 name/input。
// (每行必须是完整独立 JSON, 无换行; 这里用紧凑单行。)
const sampleTranscript = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"重构 auth 模块并写测试"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"我将派两个子代理并行处理。"},{"type":"tool_use","id":"t1","name":"Task","input":{"description":"重构 auth 模块","prompt":"重构 src/auth.ts 模块"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Task","input":{"description":"为 auth 写单元测试","prompt":"为 src/auth.ts 写单测"}}]}}
{"type":"result","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}}`

func TestExtractSubagents(t *testing.T) {
	subs := extractSubagents([]byte(sampleTranscript))
	if len(subs) != 2 {
		t.Fatalf("期望解析出 2 个子代理, 实际 %d: %+v", len(subs), subs)
	}
	// 不依赖顺序, 按 Task 内容匹配
	byTask := map[string]subagentInfo{}
	for _, s := range subs {
		byTask[s.Task] = s
	}
	a, ok := byTask["重构 auth 模块"]
	if !ok {
		t.Fatalf("缺少子代理 '重构 auth 模块', 实际: %+v", subs)
	}
	if a.Name != "Task" {
		t.Errorf("Name 期望 Task, 实际 %q", a.Name)
	}
	if a.State != "thinking" {
		t.Errorf("assistant 消息内的 tool_use 应推断为 thinking, 实际 %q", a.State)
	}
	b, ok := byTask["为 auth 写单元测试"]
	if !ok {
		t.Fatalf("缺少子代理 '为 auth 写单元测试', 实际: %+v", subs)
	}
	if b.State != "thinking" {
		t.Errorf("State 期望 thinking, 实际 %q", b.State)
	}
}

func TestExtractSubagents_IgnoresNonTask(t *testing.T) {
	data := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"ls"}},{"type":"tool_use","id":"y","name":"Task","input":{"description":"子任务A","prompt":""}}]}}`
	subs := extractSubagents([]byte(data))
	if len(subs) != 1 {
		t.Fatalf("只应解析出 1 个 Task 子代理, 实际 %d: %+v", len(subs), subs)
	}
	if subs[0].Task != "子任务A" {
		t.Errorf("Task 期望 '子任务A', 实际 %q", subs[0].Task)
	}
}

func TestExtractSubagents_Empty(t *testing.T) {
	if subs := extractSubagents([]byte("")); len(subs) != 0 {
		t.Errorf("空输入应返回空, 实际 %+v", subs)
	}
	if subs := extractSubagents([]byte("not json at all\n")); len(subs) != 0 {
		t.Errorf("非法输入应返回空, 实际 %+v", subs)
	}
}

func TestExtractSubagents_DescriptionOrPrompt(t *testing.T) {
	// 仅 prompt 无 description 时也能提取
	data := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"subagent","input":{"prompt":"review PR"}}]}}`
	subs := extractSubagents([]byte(data))
	if len(subs) != 1 || !strings.Contains(subs[0].Task, "review PR") {
		t.Errorf("应提取 prompt 作为任务, 实际 %+v", subs)
	}
}

// TestMergeTranscriptSubagents_Integration 端到端: 真实 transcript 文件 -> 写库 -> 树中可见子代理节点。
func TestMergeTranscriptSubagents_Integration(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "transcript.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Task","input":{"description":"重构 auth 模块","prompt":"x"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Task","input":{"description":"为 auth 写单元测试","prompt":"y"}}]}}`
	if err := os.WriteFile(tf, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	// 先写 root transcript 节点(rootPID 由文件名哈希决定)
	tinfo := transcriptInfo{file: tf, tool: "claude", lastTs: time.Now().UnixNano(), lastLine: "x"}
	rootPID := transcriptPID(tf)
	st.Upsert(store.Agent{PID: rootPID, Tool: "claude", State: "thinking", UpdatedAt: time.Now().Unix(), ParentPID: 0, RootPID: rootPID, Depth: 0, IsSubagent: false, Src: "transcript"})

	c := &Collector{store: st, cfg: config.Default(), monitors: map[string]*agentMonitor{}, pidOwner: map[int]int{}}
	m := &agentMonitor{tool: "claude", src: "transcript"}
	c.mergeTranscriptSubagents(m, tinfo, rootPID, time.Now(), map[int]bool{})

	tree, err := st.ListTree()
	if err != nil {
		t.Fatal(err)
	}
	var subs []store.Agent
	for _, a := range tree {
		if a.Src == "transcript" && a.IsSubagent {
			subs = append(subs, a)
		}
	}
	if len(subs) != 2 {
		t.Fatalf("期望树中 2 个 transcript 子代理节点, 实际 %d: %+v", len(subs), tree)
	}
	tasks := map[string]bool{}
	for _, s := range subs {
		if s.ParentPID != rootPID {
			t.Errorf("子代理 ParentPID 应为 root %d, 实际 %d", rootPID, s.ParentPID)
		}
		if !s.IsSubagent || s.Depth != 1 {
			t.Errorf("子代理应为 depth=1 is_subagent=true, 实际 depth=%d sub=%v", s.Depth, s.IsSubagent)
		}
		tasks[s.Task] = true
	}
	if !tasks["重构 auth 模块"] || !tasks["为 auth 写单元测试"] {
		t.Errorf("子代理任务名缺失: %+v", tasks)
	}
}
