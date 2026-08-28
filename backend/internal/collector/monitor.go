package collector

import (
	"agentmon/internal/config"
	"agentmon/internal/ebpf"
	"agentmon/internal/store"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type agentMonitor struct {
	key      string
	tool     string
	pid      int // canonical 根 pid(进程级); transcript 路径为 0
	pts      string
	cancel   context.CancelFunc
	lastOut  atomic.Int64
	mu       sync.Mutex
	lines    []string
	ringCap  int
	hasChild atomic.Bool
	ready    atomic.Bool
	src      string // "proc" 或 "transcript"
	ebpfUsed bool   // 本 agent 使用 eBPF 数据(非 pty fallback)
	// ptsBlocked: 进程当前阻塞在 pty 输入(读 stdin 无数据可消费), 用于识别"等待用户输入"
	ptsBlocked atomic.Bool
	// inputPollBlocked: Node.js 事件循环正通过 epoll 等待且 stdin 是 pts。该信号本身有歧义，
	// 只能与 transcript 中未完成的 tool_use 组合判断 Claude 是否等待授权。
	inputPollBlocked atomic.Bool
	// 状态推导参数(由 Collector 在创建 monitor 时注入, 供 AgentSource.DeriveProcState 复用,
	// 避免把 Collector 的 cfg/llmIPs 直接传给 Source 方法)。
	idleSeconds      int
	waitInputSeconds int
	llmIPs           map[string]bool
	// 行为采集(全 eBPF)状态
	behMu        sync.Mutex
	lastCmd      string // 最近一次 execve 的可执行名(basename, 用于展示)
	lastCmdLine  string // 最近一次 execve 的完整命令行(截断, 用于安全审计: 凭据/破坏性命令检测)
	lastFile     string
	lastFileWr   bool
	lastEditFile string // 最近一次写入的文件(用于 editing 状态展示, 区别于 last_file 的"最近打开")
	lastConn     string
	lastEvTs     int64 // 最近一次行为事件(纳秒)
	connTs       int64 // 最近一次 connect(纳秒)
	editTs       int64 // 最近一次编辑写文件(纳秒)
	// 时间线去重: 记录上次已上报的行为值, 变化时才写 events 表
	prevCmd      string
	prevEditFile string
	prevConn     string
	prevState    string
	// 异常检测: needsInput 首次出现的时间戳, 用于"等待输入未处理"超时判定
	needsInputSince int64
	// transcript 增量读偏移量(字节), 避免每轮全量读大文件
	transcriptOffset int64
	// transcript 子代理扫描偏移量(字节), 避免 mergeTranscriptSubagents 每轮全量读大文件
	subagentScanOffset int64
	// transcript 状态缓存: claude 空闲(文件无新增)时, 用上次解析结果代表该会话, 避免节点闪烁/退化。
	lastLine       string
	lastType       string
	lastApiStatus  string
	lastApiErr     string
	lastApiErrMsg  bool
	lastApiTs      int64 // 末条 error 行的真实时间戳(缓存用, 供 error 新鲜度判断)
	lastStopReason string
	lastToolName   string
	lastNeedsInput bool
	pendingTools   map[string]string // tool_use_id -> tool name
}

func (m *agentMonitor) setLastLine(line string) {
	m.mu.Lock()
	m.lines = []string{cleanLine(line)}
	m.mu.Unlock()
	m.lastOut.Store(time.Now().UnixNano())
}

func (m *agentMonitor) lastText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.lines) == 0 {
		return ""
	}
	s := m.lines[len(m.lines)-1]
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// consumeEvent 处理一条 eBPF 行为事件(仅元数据), 更新行为状态和活动时间戳。
// 无隐私风险(不含文件内容/pty 字节)。
// 注意: 任何 eBPF 事件(含 write/execve/openat/rename/connect)都说明进程存活且有活动,
// 必须刷新 lastOut, 否则 copilot 思考中写 pty 输出时 lastOut 仍保持旧值,
// 导致 detectAnomalies 在 120 秒后误判"卡死/无响应"。
func (m *agentMonitor) consumeEvent(ev ebpf.Event, cfg *config.Config) {
	if cfg.Behavior.Capture == "off" {
		return
	}
	m.lastOut.Store(time.Now().UnixNano())
	// eBPF 用 probe_read_user 逐字节读, 字符串后可能带残留字节, 截断到首个 NUL。
	rawArg := string(ev.Arg[:])
	if i := strings.IndexByte(rawArg, 0); i >= 0 {
		rawArg = rawArg[:i]
	}
	basename := func(s string) string {
		if i := strings.LastIndexByte(s, '/'); i >= 0 {
			return s[i+1:]
		}
		return s
	}

	m.behMu.Lock()
	m.lastEvTs = time.Now().UnixNano()
	switch ev.Type {
	case ebpf.EvExecve:
		if a := basename(rawArg); a != "" {
			m.lastCmd = a
		}
		// 完整命令行(截断)留存, 供安全审计检测凭据泄露/破坏性命令(不依赖 basename)。
		if cl := truncate(rawArg, 200); cl != "" {
			m.lastCmdLine = cl
		}
	case ebpf.EvOpenat:
		if a := basename(rawArg); a != "" && !isAgentInternalPath(rawArg) && !isKernelPseudoPath(rawArg) {
			// 仅当非临时/内部文件时才更新"最近打开"(避免 agent 自身临时文件污染展示)
			if !isTransientFile(a) {
				m.lastFile = a
			}
			m.lastFileWr = ev.WrOnly == 1
			// 仅"非瞬时/内部"文件的写操作算编辑信号(editing / 阻断 thinking)。
			// copilot 等代理会持续写随机名状态/临时文件(如 105ebccf-...、*.tmp),
			// 这些不算用户编辑, 也不应阻止 thinking 出现。
			if ev.WrOnly == 1 && !isTransientFile(a) {
				m.lastEditFile = a
				m.editTs = time.Now().UnixNano()
			}
		}
	case ebpf.EvRename:
		// rename(oldpath, newpath): newpath 是最终落盘的真实文件名。
		// copilot 等代理"写临时文件 -> rename 成真实文件", openat 只抓到临时名(已过滤),
		// 这里拿到 rename 目标(如 hello.py), 即用户真正关心的业务文件。
		if a := basename(rawArg); a != "" && !isTransientFile(a) && !isAgentInternalPath(rawArg) && !isKernelPseudoPath(rawArg) {
			m.lastFile = a
			m.lastEditFile = a
			m.lastFileWr = true
			m.editTs = time.Now().UnixNano()
		}
	case ebpf.EvConnect:
		// daddr 网络字节序 -> IPv4 字符串
		d := ev.Daddr
		ip := fmt.Sprintf("%d.%d.%d.%d", d&0xff, (d>>8)&0xff, (d>>16)&0xff, (d>>24)&0xff)
		port := (ev.Port >> 8) | ((ev.Port & 0xff) << 8) // 网络字节序转主机
		m.lastConn = fmt.Sprintf("%s:%d", ip, port)
		m.connTs = time.Now().UnixNano()
	}
	m.behMu.Unlock()
}

func (c *Collector) startProcMonitor(key string, pid int, tool string) *agentMonitor {
	pts := findPts(pid)
	_, cancel := context.WithCancel(context.Background())
	m := &agentMonitor{key: key, tool: tool, pid: pid, pts: pts, cancel: cancel, ringCap: c.cfg.Collect.PtyRing, src: "proc",
		idleSeconds: c.cfg.Collect.IdleSeconds, waitInputSeconds: c.cfg.Collect.WaitInputSeconds, llmIPs: c.llmIPs}
	// 纯只读采集: 仅用 eBPF(/proc/syscall 元数据) + probePts(FIONREAD peek / wchan, 均不消费 pty 字节)。
	// 不再 open pty 从属设备读取文本 —— 那会 drain 终端字节, 干扰被监控 agent 的键盘输入(违反"只采集不控制")。
	// pty 文本输出由 transcript(JSONL 文件, 纯文件读) 或 eBPF 行为元数据替代。
	m.ready.Store(true)
	return m
}

// updateState 由内核态信号(eBPF 行为 / LLM 连接 / pty 阻塞)推导通用状态。
// 各 agent 的专属推导见对应 AgentSource; 未注册 Source 的 agent 以本函数为兜底。
func (m *agentMonitor) updateState(idleSeconds, waitInputSeconds int, llmIPs map[string]bool) (string, string, bool, string, string, string, string) {
	now := time.Now().UnixNano()
	idleNs := int64(idleSeconds) * int64(time.Second)
	waitNs := int64(waitInputSeconds) * int64(time.Second)
	lastOut := m.lastOut.Load()
	active := (now - lastOut) < idleNs

	// 行为信号(全 eBPF)
	m.behMu.Lock()
	lastCmd := m.lastCmd
	lastFileWr := m.lastFileWr
	lastEditFile := m.lastEditFile
	lastConn := m.lastConn
	connTs := m.connTs
	editTs := m.editTs
	lastEvTs := m.lastEvTs
	m.behMu.Unlock()

	// 判定 LLM 连接: 近期 connect 到已知 LLM host(按 IP/关键字匹配),
	// 或当前进程有到 LLM host 的已建立 TCP 连接(覆盖复用/持久连接)。
	connIP := lastConn
	if i := strings.LastIndexByte(lastConn, ':'); i >= 0 {
		connIP = lastConn[:i]
	}
	isLLM := llmIPs[connIP]
	if !isLLM {
		for h := range llmIPs {
			if !strings.Contains(h, ".") && strings.Contains(lastConn, h) {
				isLLM = true
				break
			}
		}
	}
	// 复用连接检测: 直接看 /proc/<pid>/net/tcp 是否有到 LLM 的 ESTABLISHED 套接字
	if !isLLM {
		isLLM = hasLLMConn(m.pid, llmIPs)
	}
	// 兼容旧兜底: 已知 LLM 代理 + lastConn 为公网 443/80 -> 视为连 LLM(无需实时套接字)。
	if !isLLM && isOutboundTLS(lastConn) && isKnownLLMAgent(m.tool) {
		isLLM = true
	}
	// 收紧: 仅当"近期有 connect 事件"或"近期有 eBPF 行为活动"时, 才算 LLM 活跃(thinking)。
	connActive := (now - connTs) < idleNs  // 近期刚 connect
	recentBeh := (now - lastEvTs) < idleNs // 近期有 execve/openat/write 行为
	llmActive := isLLM && (connActive || recentBeh)
	// thinking 窗口放宽到 30s(LLM 推理通常 <30s, 且复用连接下 connect 事件只触发一次)
	const thinkWindow = 30 * int64(time.Second)
	llmConnected := llmActive && ((now - connTs) < thinkWindow)
	recentEdit := (now - editTs) < idleNs
	// active 收窄为"近期有写文件活动"(纯读 IO 不算 active, 否则 copilot 读配置期间无法判 thinking)
	activeWrite := recentEdit
	// 显示用文件名: 优先最近写入的文件
	file := displayFile(m.lastFile, lastEditFile, recentEdit)

	// 等待用户输入: 全面可观测模式专属信号。
	// 进程阻塞在 tty 专用等待函数(ptsBlocked, 见 probeTerminalBlock 的强信号) +
	// 近期无输出/无 eBPF 活动/未连 LLM(安静) -> 它在等你敲命令/确认, 需要提醒。
	needsInput := m.ptsBlocked.Load() && !active && !llmActive &&
		(now-lastOut) > waitNs

	var state, conf, reason string
	switch {
	case needsInput:
		// 等待用户输入: 最高优先(需要主动提醒)。具体原因由前端 i18n 渲染(state=waiting + reason 枚举)。
		state, conf, reason = "waiting", "high", store.ReasonAwaitingApproval
	case matchAny(strings.ToLower(m.lastText()), waitingWords()) && !active:
		// 输出文本命中确认词(如 Y/n / Proceed?) -> 也是等待确认
		state, conf, reason = "waiting", "medium", store.ReasonAwaitingApproval
	case llmConnected && !recentEdit && !activeWrite:
		// 连了 LLM(含复用连接)且近期无本地写活动 -> 推理中
		state, conf, reason = "thinking", "high", store.ReasonThinkingLLM
	case recentEdit && lastFileWr:
		// 近期写源码文件 -> 编辑中
		state, conf, reason = "editing", "high", ""
	case m.hasChild.Load() && m.src == "proc":
		state, conf = "running", "high"
	case active || recentBeh:
		state, conf = "running", "high"
	default:
		text := strings.ToLower(m.lastText())
		if m.src == "proc" && m.pts == "" && !m.ebpfUsed && text == "" {
			// 识别不出明确状态时归 idle(低置信度), 不再臆造 blocked —— blocked 无任 agent
			// 协议对应(copilot/claude/codex 均无 blocked 状态), 且曾造成 node blocked 噪音误报。
			state, conf = "idle", "low"
		} else {
			state, conf = "idle", "medium"
		}
	}
	return state, conf, needsInput, lastCmd, file, lastConn, reason
}

// displayFile 选择展示用的文件名: 优先最近写入的文件(editing 目标), 否则最近打开的文件。
func displayFile(lastFile, lastEditFile string, recentEdit bool) string {
	if recentEdit && lastEditFile != "" {
		return lastEditFile
	}
	return lastFile
}

// recordEvents 将行为变化写入时间线(去重: 仅当值变化才记录)。仅元数据, 无隐私风险。
func (m *agentMonitor) recordEvents(st *store.Store, pid int, cmd, file, conn, state string) {
	now := time.Now().Unix()
	if cmd != "" && cmd != m.prevCmd {
		st.RecordEvent(store.Event{PID: pid, Ts: now, Kind: "cmd", Detail: cmd})
		m.prevCmd = cmd
	}
	if file != "" && file != m.prevEditFile {
		fk := "user"
		if isTransientFile(file) {
			fk = "agent_temp"
		}
		st.RecordEvent(store.Event{PID: pid, Ts: now, Kind: "edit", Detail: file, FileKind: fk})
		m.prevEditFile = file
	}
	if conn != "" && conn != m.prevConn {
		st.RecordEvent(store.Event{PID: pid, Ts: now, Kind: "conn", Detail: conn})
		m.prevConn = conn
	}
	if state != "" && state != m.prevState {
		st.RecordEvent(store.Event{PID: pid, Ts: now, Kind: "state", Detail: state, State: store.AgentState(state)})
		m.prevState = state
	}
}

// probePts 探测该 agent 的终端阻塞信号(纯只读 /proc 元数据, 绝不打开 pty 从属设备 ——
// 打开会 drain 终端字节, 干扰被监控 agent 的键盘输入, 违反"只采集不控制"铁律)。
// 返回强信号 blocked(阻塞在 tty 专用等待函数, 确定在等键盘); 弱信号 inputPollBlocked
// 存入 m, 由调用方与转录本的未完成 tool_use 组合判定(见 claudeSource.DeriveTranscriptState)。
func (m *agentMonitor) probePts() (blocked bool) {
	if m.pts == "" || m.pid <= 0 {
		m.inputPollBlocked.Store(false)
		return false
	}
	blocked, inputPollBlocked := probeTerminalBlock("/proc", m.pid)
	m.inputPollBlocked.Store(blocked || inputPollBlocked)
	return blocked
}

// probeTerminalBlock 从 procRoot(生产为 "/proc", 测试注入临时目录)读取 pid 的终端阻塞信号。
// 返回:
//
//	blocked          强信号 —— wchan 命中 tty 专用等待函数(n_tty_read 等), 确定在等键盘输入
//	inputPollBlocked 弱信号 —— wchan 在 epoll 等待且 stdin 指向 pts。Node.js 系 agent 等 LLM
//	                 回复时同样处于该状态, 本身有歧义, 调用方不得单独据此判定"等待输入"
//
// 两者均要求进程处于睡眠态(S/D): 进程在跑(R/Z/T)说明 wchan 是采样残留, 一律否定。
func probeTerminalBlock(procRoot string, pid int) (blocked, inputPollBlocked bool) {
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	// wchan 判定进程阻塞点。仅终端专用等待函数(如 n_tty_read)才判为"等用户输入"。
	// 不含 poll/read 等通用等待(agent 等 LLM 回复也在 poll socket, 不应误标)。
	if data, err := os.ReadFile(filepath.Join(base, "wchan")); err == nil {
		w := strings.TrimSpace(string(data))
		if strings.Contains(w, "n_tty") || strings.Contains(w, "tty") {
			blocked = true
		}
		// Linux 对 epoll_wait 的 wchan 符号是 ep_poll(下划线), 部分内核/运行时显示 epoll_*。
		// 两种拼写都要覆盖 —— 只匹配 "epoll" 会漏掉最常见的 ep_poll(历史 bug)。
		if strings.Contains(w, "ep_poll") || strings.Contains(w, "epoll") {
			if link, err := os.Readlink(filepath.Join(base, "fd", "0")); err == nil && strings.Contains(link, "pts") {
				inputPollBlocked = true
			}
		}
	}
	// S/D 只说明进程在睡眠(等网络/等子进程/等定时器同样如此), 绝不能据此正向判定"等终端输入"。
	// 历史 bug: 这里曾无条件 blocked=true, 使上面两条 wchan 判据完全失效, 正在推理的 claude
	// 被误报"等待用户输入"。现仅作反向否定使用。
	if data, err := os.ReadFile(filepath.Join(base, "stat")); err == nil {
		if st := procStatState(string(data)); st != "" && st != "S" && st != "D" {
			blocked = false
			inputPollBlocked = false
		}
	}
	return blocked, inputPollBlocked
}

// procStatState 取 /proc/<pid>/stat 的第 3 字段(进程状态位 R/S/D/Z/T)。
// comm(第 2 字段)被 () 包裹且允许含空格与 ')' —— 系统上真实存在 comm 为 "(sd-pam)" 的进程,
// 其 stat 形如 `123 ((sd-pam)) S 1 ...`。故必须用最后一个 ')' 定位, 用第一个会解析到 ")" 而误判。
func procStatState(s string) string {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
