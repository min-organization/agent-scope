package collector

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestClaudeProjectHash 验证 cwd -> project hash 编码规则(与 claude 自身一致: 所有 "/" 替换为 "-")。
func TestClaudeProjectHash(t *testing.T) {
	cases := map[string]string{
		"/data/docker/compose/agent-mon":         "-data-docker-compose-agent-mon",
		"/data/docker/compose/agent-mon/backend": "-data-docker-compose-agent-mon-backend",
		"/tmp/amt-e2e":                           "-tmp-amt-e2e",
	}
	for cwd, want := range cases {
		if got := claudeProjectHash(cwd); got != want {
			t.Fatalf("claudeProjectHash(%q)=%q, want %q", cwd, got, want)
		}
	}
}

// TestActiveClaudeProjects 验证: 活 claude 进程(cwd 匹配)的 project 被识别。
// 起一个 comm=claude 且 cwd=dir 的子进程(若运行环境无法设 comm, 则跳过该子部分, 仅保留纯函数校验)。
func TestActiveClaudeProjects(t *testing.T) {
	dir := t.TempDir()
	wrapped := exec.Command("bash", "-c", "exec -a claude sleep 30")
	wrapped.Dir = dir
	if err := wrapped.Start(); err != nil {
		t.Fatal(err)
	}
	defer wrapped.Process.Kill()

	// 等子进程 exec 完成(argv[0] 生效), 轮询最多 3s。
	var hash string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// 确认子进程 comm 是否真的变成 claude(部分环境会剥离 argv[0] 改写)。
		commB, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", wrapped.Process.Pid))
		if string(commB) == "claude\n" || string(commB) == "claude" {
			hash = claudeProjectHash(dir)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if hash == "" {
		t.Skip("运行环境无法将子进程 comm 设为 claude(exec -a 被剥离), 跳过活进程匹配子测试(纯函数 TestClaudeProjectHash 已覆盖)")
	}

	// 轮询等待 activeClaudeProjects 识别到该 project(子进程需被 /proc 扫描到)。
	for time.Now().Before(deadline) {
		if _, ok := activeClaudeProjects()[hash]; ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("期望 activeClaudeProjects 含 %q", hash)
}
