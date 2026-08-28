package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentmon/internal/config"
	"agentmon/internal/store"
)

// realPIDs 返回当前进程中可用于测试的真实 PID 列表。
// 遍历 /proc/self/task/, 将线程 TID 转为 PID——
// 这些 PID 一定在 /proc 中存在, 确保 processExists 通过。
func realPIDs(t *testing.T) []int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatalf("ReadDir /proc/self/task: %v", err)
	}
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	if len(pids) < 5 {
		t.Fatalf("需要至少 5 个可用 PID, 当前仅 %d 个", len(pids))
	}
	return pids
}

// TestDetectAnomaliesSecurity 验证 P0-2 安全审计: 凭据泄露 / 破坏性命令检测。
// 直接走真实 detectAnomalies 代码路径(复用 fireAlert + store.RecordAlert)。
// 每个用例用独立 pid, 避免自愈 DeleteAlertsKind 跨用例互相清除。
// 命令行字符串须匹配 config.Default().Alert.[SecretPatterns|DestructiveKeywords] 中的实际模式。
func TestDetectAnomaliesSecurity(t *testing.T) {
	cfg := config.Default()
	if len(cfg.Alert.DestructiveKeywords) == 0 {
		t.Fatal("DestructiveKeywords 默认值为空")
	}
	if len(cfg.Alert.SecretPatterns) == 0 {
		t.Fatal("SecretPatterns 默认值为空")
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	c := &Collector{cfg: cfg, store: st, lastAlert: make(map[string]int64)}
	pids := realPIDs(t)

	hasKind := func(pid int, kind string) bool {
		al, _ := st.RecentAlerts(100)
		for _, a := range al {
			if a.PID == pid && a.Kind == kind {
				return true
			}
		}
		return false
	}
	levelOf := func(pid int, kind string) string {
		al, _ := st.RecentAlerts(100)
		for _, a := range al {
			if a.PID == pid && a.Kind == kind {
				return a.Level
			}
		}
		return ""
	}

	// 1) 凭据泄露: 命令行含 api_key= 模式
	p1 := pids[0]
	c.detectAnomalies(&agentMonitor{tool: "hermes", lastCmdLine: `export API_KEY=sk-proj-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`, lastEditFile: ""}, p1, "running", false, "")
	if !hasKind(p1, "secret_leak") {
		t.Error("secret_leak 未触发(命令行含 api_key=)")
	}
	if levelOf(p1, "secret_leak") != "warning" {
		t.Errorf("secret_leak 应为 warning, 实际 %q", levelOf(p1, "secret_leak"))
	}

	// 2) 破坏性命令: rm -rf
	p2 := pids[1]
	c.detectAnomalies(&agentMonitor{tool: "hermes", lastCmdLine: "bash -c 'rm -rf /data/important && git push --force'", lastEditFile: ""}, p2, "running", false, "")
	if !hasKind(p2, "destructive_cmd") {
		t.Error("destructive_cmd 未触发(rm -rf 命中)")
	}
	if levelOf(p2, "destructive_cmd") != "critical" {
		t.Errorf("destructive_cmd 应为 critical, 实际 %q", levelOf(p2, "destructive_cmd"))
	}

	// 3) 凭据文件写: .env
	p3 := pids[2]
	c.detectAnomalies(&agentMonitor{tool: "hermes", lastCmdLine: "touch /app/.env", lastEditFile: ".env"}, p3, "running", false, "")
	if !hasKind(p3, "secret_leak") {
		t.Error("secret_leak 未触发(凭据文件 .ev 写)")
	}

	// 4) 良性命令: 不应产生任何告警(独立 pid)
	p4 := pids[3]
	c.detectAnomalies(&agentMonitor{tool: "hermes", lastCmdLine: "bash -c 'ls -la && echo done'", lastEditFile: "readme.md"}, p4, "running", false, "")
	if hasKind(p4, "secret_leak") || hasKind(p4, "destructive_cmd") {
		t.Error("良性命令不应触发 secret_leak/destructive_cmd")
	}

	// 5) 自愈: p1 的命令变为良性后应清除其 secret_leak 告警
	c.detectAnomalies(&agentMonitor{tool: "hermes", lastCmdLine: "bash -c 'echo hi'", lastEditFile: "x.md"}, p1, "running", false, "")
	if hasKind(p1, "secret_leak") {
		t.Error("自愈失败: p1 的 secret_leak 告警未随命令变良性而清除")
	}
	// p2/p3 仍应保持(它们没被改回良性)
	if !hasKind(p2, "destructive_cmd") {
		t.Error("自愈误伤: p2 的 destructive_cmd 不应被清除")
	}
	if !hasKind(p3, "secret_leak") {
		t.Error("自愈误伤: p3 的 secret_leak 不应被清除")
	}

	t.Log("P0-2 安全审计检测全部通过")
}

// TestSecretAlertDoesNotLeakPayload 验证告警正文绝不携带凭据本体。
//
// 为什么这条测试必须存在: 告警一旦生成就会 (1) 明文落 SQLite (2) POST 到配置的
// webhook —— 离开本机 (3) 写入 0644 全局可读日志 (4) 经无鉴权 /api/alerts 暴露。
// 而 secret_leak 的触发条件恰恰是"命令行里有凭据", 若把命令行原文塞进 message,
// 这个安全功能本身就成了最大的凭据外泄渠道, 也击穿了项目"仅采集元数据"的承诺。
func TestSecretAlertDoesNotLeakPayload(t *testing.T) {
	const secretValue = "sk-proj-DEADBEEFsupersecretVALUE123"

	cfg := config.Default()
	dbPath := filepath.Join(t.TempDir(), "leak.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	c := &Collector{cfg: cfg, store: st, lastAlert: make(map[string]int64)}
	pids := realPIDs(t)

	messageOf := func(pid int, kind string) string {
		al, _ := st.RecentAlerts(100)
		for _, a := range al {
			if a.PID == pid && a.Kind == kind {
				return a.Message
			}
		}
		return ""
	}

	// 1) secret_leak: 命令行带 Bearer 令牌 -> 告警不得含令牌值, 但要能定位到进程与模式
	p1 := pids[0]
	c.detectAnomalies(&agentMonitor{tool: "claude",
		lastCmdLine: "curl -H 'Authorization: Bearer " + secretValue + "' https://api.example.com",
	}, p1, "running", false, "")
	msg := messageOf(p1, "secret_leak")
	if msg == "" {
		t.Fatal("secret_leak 未触发(命令行含 'Bearer ')")
	}
	if strings.Contains(msg, secretValue) {
		t.Fatalf("告警正文泄露了凭据本体! message=%q", msg)
	}
	// 仍需可定位: 含可执行名(curl)与命中的模式名
	if !strings.Contains(msg, "curl") {
		t.Errorf("告警应含可执行名以便定位, 实际 %q", msg)
	}
	if !strings.Contains(msg, "Bearer") {
		t.Errorf("告警应含命中的模式名(来自配置, 非密钥), 实际 %q", msg)
	}

	// 2) 文件名命中(.env): 文件名本身不是凭据, 应照常展示
	p2 := pids[1]
	c.detectAnomalies(&agentMonitor{tool: "claude",
		lastCmdLine: "python3 load_config.py", lastEditFile: ".env",
	}, p2, "running", false, "")
	if m := messageOf(p2, "secret_leak"); !strings.Contains(m, ".env") {
		t.Errorf("文件名命中应展示文件名, 实际 %q", m)
	}

	// 3) destructive_cmd: 必须保留命令结构(rm -rf 的目标是告警全部价值),
	//    但夹带的凭据取值要被掩码
	p3 := pids[2]
	c.detectAnomalies(&agentMonitor{tool: "claude",
		lastCmdLine: "bash -c 'rm -rf /data/important --token=" + secretValue + "'",
	}, p3, "running", false, "")
	dmsg := messageOf(p3, "destructive_cmd")
	if dmsg == "" {
		t.Fatal("destructive_cmd 未触发")
	}
	if strings.Contains(dmsg, secretValue) {
		t.Fatalf("破坏性命令告警泄露了凭据本体! message=%q", dmsg)
	}
	if !strings.Contains(dmsg, "/data/important") {
		t.Errorf("破坏性命令告警必须保留操作目标(否则无法判断危害), 实际 %q", dmsg)
	}
}

// TestScrubSecrets 校验掩码器: 保留模式名与命令结构, 只掩掉取值。
func TestScrubSecrets(t *testing.T) {
	patterns := config.Default().Alert.SecretPatterns
	cases := map[string]string{
		"--password=hunter2 --verbose":       "--password=*** --verbose",
		"export API_KEY=sk-abc123":           "export API_KEY=*** ", // 尾部无内容
		"Authorization: Bearer eyJhbGciOi":   "Authorization: Bearer ***",
		"aws --key AKIAIOSFODNN7EXAMPLE end": "aws --key AKIA*** end",
		"ls -la /tmp":                        "ls -la /tmp", // 无命中 -> 原样
		"":                                   "",
	}
	for in, want := range cases {
		got := scrubSecrets(in, patterns)
		// export API_KEY= 用例: 模式 api_key= 命中后取值被掩, 允许尾随空白差异
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			t.Errorf("scrubSecrets(%q)\n  实际 %q\n  期望 %q", in, got, want)
		}
		if strings.Contains(got, "hunter2") || strings.Contains(got, "sk-abc123") ||
			strings.Contains(got, "eyJhbGciOi") || strings.Contains(got, "IOSFODNN7EXAMPLE") {
			t.Errorf("scrubSecrets(%q) 残留凭据: %q", in, got)
		}
	}
}

// TestExecNameOf 校验只取可执行名(不带任何参数, 参数可能夹带凭据)。
func TestExecNameOf(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/curl -H 'Bearer xyz'": "curl",
		"bash -c 'rm -rf /'":            "bash",
		"python3":                       "python3",
		"  ":                            "?",
		"":                              "?",
	}
	for in, want := range cases {
		if got := execNameOf(in); got != want {
			t.Errorf("execNameOf(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
