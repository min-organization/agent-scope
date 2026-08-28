package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentmon/internal/store"
)

// 写一段 events.jsonl 到临时 session 目录, 返回该目录路径。
func writeCopilotSession(t *testing.T, name string, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	sess := filepath.Join(dir, name)
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseCopilotStateUserInputRequested(t *testing.T) {
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"assistant.turn_start","data":{"turnId":"0"}}`,
		`{"type":"assistant.message","data":{"phase":"final_answer"}}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"}}`,
		`{"type":"user_input.requested","data":{}}`,
	})
	st, task, _, ok := parseCopilotStateInDir(dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if st != "waiting" {
		t.Errorf("state = %q, want waiting", st)
	}
	if task != "" {
		t.Errorf("user_input 等待 task 应为空(语义由 reason 枚举承载, 不硬编码中文), got %q", task)
	}
	if reason := parseCopilotStateResultInDir(dir).reason; reason != store.ReasonAwaitingInput {
		t.Errorf("user_input reason = %q, want awaiting_input", reason)
	}
}

func TestParseCopilotStateParallelRequests(t *testing.T) {
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"permission.requested","data":{"requestId":"p1","toolName":"bash"}}`,
		`{"type":"permission.requested","data":{"requestId":"p2","toolName":"edit"}}`,
		`{"type":"permission.completed","data":{"requestId":"p2"}}`,
	})
	r := parseCopilotStateResultInDir(dir)
	if r.state != "waiting" || r.reason != store.ReasonAwaitingApproval || r.task != "bash" || !r.needsInput {
		t.Fatalf("p2 完成后 p1 仍应等待, 实际 %+v", r)
	}
}

func TestParseCopilotStateParallelTools(t *testing.T) {
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"edit","arguments":{"path":"/first.go"}}}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"c2","toolName":"view","arguments":{"path":"/second.go"}}}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"c2"}}`,
	})
	r := parseCopilotStateResultInDir(dir)
	if r.state != "running" || r.file != "/first.go" || r.needsInput {
		t.Fatalf("c2 完成后 c1 仍应执行, 实际 %+v", r)
	}
}

func TestParseCopilotStateSelectsMatchingCWD(t *testing.T) {
	dir := t.TempDir()
	write := func(name, cwd, event string, age time.Duration) {
		sess := filepath.Join(dir, name)
		if err := os.MkdirAll(sess, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(sess, "events.jsonl")
		content := `{"type":"session.start","data":{"context":{"cwd":"` + cwd + `"}}}` + "\n" + event + "\n"
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	write("other", "/repo/other", `{"type":"permission.requested","data":{"requestId":"p","toolName":"bash"}}`, time.Second)
	write("wanted", "/repo/wanted", `{"type":"assistant.turn_end","data":{}}`, 10*time.Second)

	r := parseCopilotStateResultInDirForCWD(dir, "/repo/wanted")
	if r.state != "idle" {
		t.Fatalf("应选择 cwd 匹配的会话, 实际 %+v", r)
	}
}

func TestParseCopilotStateUserInputCompleted(t *testing.T) {
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"assistant.turn_start","data":{"turnId":"0"}}`,
		`{"type":"user_input.requested","data":{}}`,
		`{"type":"user_input.completed","data":{}}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"view","arguments":{"path":"/x.go"}}}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"c1"}}`,
	})
	st, _, _, _ := parseCopilotStateInDir(dir)
	if st == "waiting" {
		t.Errorf("state = waiting, want not-waiting (request was completed)")
	}
}

func TestParseCopilotStateExitPlanMode(t *testing.T) {
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"exit_plan_mode.requested","data":{}}`,
	})
	st, task, _, ok := parseCopilotStateInDir(dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if st != "waiting" {
		t.Errorf("state = %q, want waiting", st)
	}
	if task != "" {
		t.Errorf("exit_plan_mode 等待 task 应为空(语义由 reason 枚举承载), got %q", task)
	}
}

func TestParseCopilotStateElicitation(t *testing.T) {
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"elicitation.requested","data":{}}`,
	})
	st, task, _, ok := parseCopilotStateInDir(dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if st != "waiting" || task != "" {
		t.Errorf("state=%q task=%q, want waiting / 空(语义由 reason 枚举承载)", st, task)
	}
}

func TestParseCopilotStateShutdownExcluded(t *testing.T) {
	// 两个 session: s_shut 是已关闭(mtime 更新但末事件 shutdown), s_active 是活跃。
	// 应选中 s_active 而非 s_shut。
	dir := t.TempDir()
	mk := func(name string, lines []string, modAge int) string {
		sess := filepath.Join(dir, name)
		os.MkdirAll(sess, 0o755)
		content := ""
		for _, l := range lines {
			content += l + "\n"
		}
		p := filepath.Join(sess, "events.jsonl")
		os.WriteFile(p, []byte(content), 0o644)
		// 调整 mtime: modAge 越小越新
		mt := time.Now().Add(-time.Duration(modAge) * time.Second)
		os.Chtimes(p, mt, mt)
		return p
	}
	mk("s_shut", []string{
		`{"type":"assistant.turn_start","data":{"turnId":"0"}}`,
		`{"type":"session.shutdown","data":{"type":"routine"}}`,
	}, 1) // 最新
	mk("s_active", []string{
		`{"type":"assistant.turn_start","data":{"turnId":"0"}}`,
		`{"type":"permission.requested","data":{"toolName":"bash"}}`,
	}, 10) // 较旧但未关闭

	st, task, _, ok := parseCopilotStateInDir(dir)
	if !ok {
		t.Fatal("expected ok")
	}
	if st != "waiting" {
		t.Errorf("state = %q, want waiting (s_active selected, not s_shut)", st)
	}
	if task != "bash" {
		t.Errorf("task = %q, want bash (中性工具名, 语义由 reason 承载)", task)
	}
}

func TestParseCopilotStateCompletedThenActiveTool(t *testing.T) {
	// 真实场景: permission.requested -> completed -> 继续干活(tool 未完成) -> 应判 running
	dir := writeCopilotSession(t, "s1", []string{
		`{"type":"permission.requested","data":{"toolName":"read"}}`,
		`{"type":"permission.completed","data":{}}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"view","arguments":{"path":"/a.go"}}}`,
		// 注意: 无 tool.execution_complete -> 工具仍在进行
	})
	st, _, _, _ := parseCopilotStateInDir(dir)
	if st != "running" {
		t.Errorf("state = %q, want running (permission done, tool in progress)", st)
	}
}
