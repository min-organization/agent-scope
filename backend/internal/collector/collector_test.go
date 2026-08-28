package collector

import (
	"os"
	"testing"
	"time"

	"agentmon/internal/store"
)

func TestCleanLine(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\x01", ""},                             // 单控制字符
		{"\x1b[32mhello\x1b[0m", "hello"},        // ANSI 颜色
		{"\x1b[?25hworking\x1b[?25l", "working"}, // 光标序列
		{"working-EBPF\r\n", "working-EBPF"},     // CRLF
		{"Proceed? [Y/n] ", "Proceed? [Y/n]"},    // 权限词保留
		{"中文测试\x1b[31m红\x1b[0m", "中文测试红"},        // 中文保留
	}
	for _, c := range cases {
		if got := cleanLine(c.in); got != c.want {
			t.Errorf("cleanLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

var llmHosts = map[string]bool{"openai.com": true, "anthropic.com": true, "copilot": true, "codex": true}

func TestUpdateStateRunningActive(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200}
	m.setLastLine("working")
	m.lastOut.Store(time.Now().UnixNano()) // 刚刚有输出 -> active
	state, _, _, _, _, _, _ := m.updateState(5, 8, llmHosts)
	if state != "running" {
		t.Fatalf("active 应为 running, got %s", state)
	}
}

func TestUpdateStateWaiting(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200}
	m.setLastLine("agent-sim Proceed? [Y/n] ")
	m.lastOut.Store(0) // 长时间无输出 -> 非 active
	state, _, _, _, _, _, _ := m.updateState(5, 8, llmHosts)
	if state != "waiting" {
		t.Fatalf("含 Proceed? 应为 waiting, got %s", state)
	}
}

func TestUpdateStateIdle(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200}
	m.setLastLine("$ ") // 提示符, 无等待词
	m.lastOut.Store(0)
	state, _, _, _, _, _, _ := m.updateState(5, 8, llmHosts)
	if state != "idle" {
		t.Fatalf("提示符停留应为 idle, got %s", state)
	}
}

func TestUpdateStateChildRunning(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200}
	m.lastOut.Store(0)
	m.hasChild.Store(true) // 有子进程 -> 强 running 信号
	state, _, _, _, _, _, _ := m.updateState(5, 8, llmHosts)
	if state != "running" {
		t.Fatalf("有子进程应为 running, got %s", state)
	}
}

func TestUpdateStateNoPtsNoEbpfIdle(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200, pts: ""} // 无 pty 且无 eBPF
	m.lastOut.Store(0)
	state, conf, _, _, _, _, _ := m.updateState(5, 8, llmHosts)
	if state != "idle" || conf != "low" {
		t.Fatalf("无 pty 无 eBPF 应归 idle/low(不再臆造 blocked), got %s/%s", state, conf)
	}
}

// 行为采集: 近期写源码文件 -> editing
func TestUpdateStateEditing(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200}
	m.lastOut.Store(0)
	now := time.Now().UnixNano()
	m.behMu.Lock()
	m.lastFile = "auth/login.py"
	m.lastEditFile = "auth/login.py"
	m.lastFileWr = true
	m.editTs = now
	m.lastEvTs = now
	m.behMu.Unlock()
	state, _, _, _, file, _, reason := m.updateState(5, 8, llmHosts)
	if state != "editing" {
		t.Fatalf("近期写源码应为 editing, got %s", state)
	}
	if file != "auth/login.py" || reason != "" {
		t.Fatalf("editing 状态错误: file=%q reason=%q(应为空, 语义由 reason 枚举承载)", file, reason)
	}
}

// 行为采集: 连 LLM 且之后无本地活动 -> thinking
func TestUpdateStateThinking(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200}
	m.lastOut.Store(0)
	now := time.Now().UnixNano()
	// 用 LLM IP(解析后的集合匹配)
	llmMap := map[string]bool{"1.2.3.4": true, "copilot": true}
	m.behMu.Lock()
	m.lastConn = "1.2.3.4:443"
	m.connTs = now
	m.lastEvTs = now
	m.behMu.Unlock()
	state, _, _, _, _, conn, reason := m.updateState(5, 8, llmMap)
	if state != "thinking" {
		t.Fatalf("连 LLM 且无本地活动应为 thinking, got %s", state)
	}
	if conn != "1.2.3.4:443" || reason != store.ReasonThinkingLLM {
		t.Fatalf("thinking 状态错误: conn=%q reason=%q(应为 thinking_llm)", conn, reason)
	}
}

// 复用连接路径: lastConn 为空, 但 /proc/<pid>/net/tcp 有到 LLM 的 ESTABLISHED 套接字
func TestUpdateStateThinkingReusedConn(t *testing.T) {
	// 造一个临时 net/tcp: 1.2.3.4:443 小端 = 04030201:01BB, 状态 01(ESTABLISHED)
	tmp := t.TempDir()
	tcpPath := tmp + "/tcp"
	content := "  sl  local_address rem_address   st tx_queue\n   0: 0100007F:0050 04030201:01BB 01 00000000\n"
	if err := os.WriteFile(tcpPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	llmMap := map[string]bool{"1.2.3.4": true}
	// hasLLMConnInFile 直接验证复用连接检测
	if !hasLLMConnInFile(tcpPath, llmMap) {
		t.Fatalf("hasLLMConnInFile 应检测到 LLM 复用连接")
	}
}

// 宽松兜底: LLM 代理工具外连公网 443 即判 thinking(即使 host 未列在 LLMHosts)
func TestUpdateStateThinkingOutboundFallback(t *testing.T) {
	// 已知 LLM 代理(codex)外连公网 443 -> 即便无实时套接字也应判 thinking(覆盖 copilot 现已改用 events.jsonl 判定, 此处测 codex 的 LLM 连接兜底)。
	m := &agentMonitor{src: "proc", tool: "codex", ringCap: 200}
	m.lastOut.Store(0)
	now := time.Now().UnixNano()
	llmMap := map[string]bool{"openai.com": true} // 不含 github IP
	m.behMu.Lock()
	m.lastConn = "140.82.114.22:443" // GitHub Copilot 真实 IP, 公网 443
	m.connTs = now
	m.lastEvTs = now // 推理期间仍在读(纯读, 非写)
	m.behMu.Unlock()
	state, _, _, _, _, conn, reason := m.updateState(5, 8, llmMap)
	if state != "thinking" {
		t.Fatalf("codex 外连公网 443 应为 thinking, got %s", state)
	}
	if conn != "140.82.114.22:443" || reason != store.ReasonThinkingLLM {
		t.Fatalf("thinking 状态错误: conn=%q reason=%q(应为 thinking_llm)", conn, reason)
	}
}

func TestIsOutboundTLS(t *testing.T) {
	if !isOutboundTLS("140.82.114.22:443") {
		t.Error("公网 443 应为 true")
	}
	if isOutboundTLS("127.0.0.1:443") {
		t.Error("回环应为 false")
	}
	if isOutboundTLS("192.168.1.1:443") {
		t.Error("私网应为 false")
	}
	if isOutboundTLS("140.82.114.22:22") {
		t.Error("非 443/80 应为 false")
	}
}

func TestUpdateStateWaitingInput(t *testing.T) {
	m := &agentMonitor{src: "proc", ringCap: 200, pid: 12345, pts: "/dev/pts/9"}
	m.lastOut.Store(0)       // 长时间无输出 -> 安静
	m.ptsBlocked.Store(true) // 阻塞在 pty 输入
	// 无 eBPF 活动、无 LLM 连接 -> 应判 waiting(等待用户输入)
	state, conf, needs, _, _, _, _ := m.updateState(5, 8, map[string]bool{})
	if state != "waiting" || !needs {
		t.Fatalf("等待输入应为 waiting+needsInput, got %s needs=%v", state, needs)
	}
	if conf != "high" {
		t.Fatalf("等待输入置信应为 high, got %s", conf)
	}
}

func TestUpdateStateWaitingInputNotWhenActive(t *testing.T) {
	now := time.Now().UnixNano()
	m := &agentMonitor{src: "proc", ringCap: 200, pid: 12345, pts: "/dev/pts/9"}
	m.lastOut.Store(now) // 刚刚有输出 -> active, 不应判等待输入
	m.ptsBlocked.Store(true)
	state, _, needs, _, _, _, _ := m.updateState(5, 8, map[string]bool{})
	if needs {
		t.Fatalf("活跃期不应判 needsInput, got %s", state)
	}
}

func TestHexToIPv4(t *testing.T) {
	cases := map[string]string{
		"6E0CA68C": "140.166.12.110", // 小端
		"04030201": "1.2.3.4",
		"0100007F": "127.0.0.1",
	}
	for in, want := range cases {
		if got := hexToIPv4(in); got != want {
			t.Errorf("hexToIPv4(%q) = %q, want %q", in, got, want)
		}
	}
	if hexToIPv4("xyz") != "" {
		t.Error("非法输入应返回空串")
	}
}
