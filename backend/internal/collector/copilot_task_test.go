package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummarizeCopilotArgs(t *testing.T) {
	// 字符串: apply_patch 提取文件名
	got := summarizeCopilotArgs("*** Begin Patch\n*** Add File: report.txt\n+ops-agent\n*** End Patch")
	if got != "report.txt" {
		t.Errorf("apply_patch 应提取文件名, got %q", got)
	}
	// map: view 提取 path
	got = summarizeCopilotArgs(map[string]interface{}{"path": "/tmp/abtest/x.py"})
	if got != "/tmp/abtest/x.py" {
		t.Errorf("view 应提取 path, got %q", got)
	}
	// 长文本截断
	got = summarizeCopilotArgs("这是一段非常非常非常非常非常非常非常非常非常非常非常长的纯文本参数需要被截断处理一下")
	if len(got) > 63 {
		t.Errorf("过长应截断, got len %d: %q", len(got), got)
	}
}

func TestParseCopilotStateInDir(t *testing.T) {
	dir := t.TempDir()
	sess := filepath.Join(dir, "sess-1")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	// 场景: ask_user 未完成 -> 真正等待用户输入(含问题首句作 file)
	ev := `{"type":"assistant.turn_start","data":{}}
{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"ask_user","arguments":{"question":"是否立即重新构建并重启服务?","choices":["是","否"]}}}`
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(ev), 0o644); err != nil {
		t.Fatal(err)
	}
	st, task, file, needs := parseCopilotStateInDir(dir)
	if st != "waiting" || !needs {
		t.Errorf("ask_user 未完成应为 waiting+needsInput, got state=%q needs=%v", st, needs)
	}
	if task != "ask_user" {
		t.Errorf("ask_user 等待 task 应为工具名(中性数据), got %q", task)
	}
	if !strings.Contains(file, "重新构建") {
		t.Errorf("file 应为问题首句(中性数据), got %q", file)
	}

	// 场景: edit 文件 -> running + 文件名为操作文件
	ev2 := `{"type":"tool.execution_start","data":{"toolCallId":"c2","toolName":"edit","arguments":{"path":"/data/agent-mon/collector.go","old_str":"x","new_str":"y"}}}`
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(ev2), 0o644); err != nil {
		t.Fatal(err)
	}
	st, task, file, needs = parseCopilotStateInDir(dir)
	if st != "running" || needs {
		t.Errorf("edit 未完成应为 running+非needsInput, got state=%q needs=%v", st, needs)
	}
	if file != "/data/agent-mon/collector.go" {
		t.Errorf("file 应为操作文件, got %q", file)
	}
	if !strings.Contains(task, "collector.go") {
		t.Errorf("task 应含文件名, got %q", task)
	}

	// 场景: bash -> 命令摘要作 file
	ev3 := `{"type":"tool.execution_start","data":{"toolCallId":"c3","toolName":"bash","arguments":{"command":"sed -n '1,10p' /data/agent-mon/collector.go","description":"show"}}}`
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(ev3), 0o644); err != nil {
		t.Fatal(err)
	}
	st, task, file, needs = parseCopilotStateInDir(dir)
	if st != "running" || needs {
		t.Errorf("bash 未完成应为 running, got state=%q needs=%v", st, needs)
	}
	if !strings.Contains(file, "sed -n") {
		t.Errorf("file 应为命令摘要, got %q", file)
	}

	// 场景: 工具已完成 + final_answer -> thinking(总结回答)
	ev4 := `{"type":"tool.execution_start","data":{"toolCallId":"c2","toolName":"edit","arguments":{"path":"/x"}}}
{"type":"tool.execution_complete","data":{"toolCallId":"c2","success":true}}
{"type":"assistant.message","data":{"phase":"final_answer"}}`
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(ev4), 0o644); err != nil {
		t.Fatal(err)
	}
	st, task, _, needs = parseCopilotStateInDir(dir)
	if st != "thinking" || needs {
		t.Errorf("final_answer 应为 thinking+非needsInput, got state=%q needs=%v", st, needs)
	}
	if task != "" {
		t.Errorf("thinking(final_answer) task 应为空(语义由 reason 枚举承载, 不硬编码中文), got %q", task)
	}

	// 场景: permission.requested 未完成 -> waiting(等待授权), file 取最近工具命令
	ev5 := `{"type":"tool.execution_start","data":{"toolCallId":"c9","toolName":"bash","arguments":{"command":"rm -rf /tmp/foo","description":"clean"}}}
{"type":"permission.requested","data":{"toolName":"bash"}}`
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(ev5), 0o644); err != nil {
		t.Fatal(err)
	}
	st, task, file, needs = parseCopilotStateInDir(dir)
	if st != "waiting" || !needs {
		t.Errorf("permission 未完成应为 waiting+needsInput, got state=%q needs=%v", st, needs)
	}
	if task != "bash" {
		t.Errorf("permission 等待 task 应为工具名(中性数据 bash), got %q", task)
	}
	if file != "" {
		t.Errorf("permission 事件无文件字段, file 应为空, got %q", file)
	}

	// 场景: 一轮结束(turn_end), 无进行中工具 -> idle(空闲, 不再误显 thinking)
	ev6 := `{"type":"tool.execution_start","data":{"toolCallId":"c2","toolName":"edit","arguments":{"path":"/data/agent-mon/collector.go"}}}
{"type":"tool.execution_complete","data":{"toolCallId":"c2","success":true}}
{"type":"assistant.turn_end","data":{}}`
	if err := os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(ev6), 0o644); err != nil {
		t.Fatal(err)
	}
	st, task, file, needs = parseCopilotStateInDir(dir)
	if st != "idle" || needs {
		t.Errorf("turn_end 后应为 idle+非needsInput, got state=%q needs=%v", st, needs)
	}
	if task != "" {
		t.Errorf("idle 时 task 应为空, got %q", task)
	}
}
