package collector

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// procNode 是 /proc 进程树的一个节点。
type procNode struct {
	pid     int
	ppid    int
	comm    string
	cmdline string
}

// buildProcTree 一次性扫描 /proc 构建整棵进程树(全层), 返回 pid -> 节点 映射。
func buildProcTree() map[int]procNode {
	out := make(map[int]procNode)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid := readPpid(pid)
		comm := readComm(pid)
		cmd := readCmdline(pid)
		out[pid] = procNode{pid: pid, ppid: ppid, comm: comm, cmdline: cmd}
	}
	return out
}

// isExcluded 判断进程是否应被排除(自身或祖先命中排除列表)。
// 匹配规则: 仅当进程名(comm)等于排除项, 或其可执行文件 basename 等于排除项时才排除。
// 不做"整条 cmdline 包含"匹配 —— 否则像 hermes 终端包装脚本(cmdline 含 /data/.../agent-mon/backend 路径)
// 会被误判为排除项, 导致从该终端启动的 copilot 等代理被错误排除。
func isExcluded(tree map[int]procNode, pid int, exclude []string) bool {
	seen := 0
	for p := pid; p != 0 && p != 1; {
		n, ok := tree[p]
		if !ok {
			break
		}
		cc := strings.ToLower(n.comm)
		// 可执行文件 basename(取 cmdline 第一个参数的 basename)
		base := ""
		if i := strings.IndexByte(n.cmdline, ' '); i >= 0 {
			base = filepath.Base(strings.TrimSpace(n.cmdline[:i]))
		} else if n.cmdline != "" {
			base = filepath.Base(strings.TrimSpace(n.cmdline))
		}
		base = strings.ToLower(base)
		for _, ex := range exclude {
			exl := strings.ToLower(ex)
			if cc == exl || base == exl {
				return true
			}
		}
		seen++
		if seen > 50 {
			break
		}
		p = n.ppid
	}
	return false
}

// descendantPids 返回 pid 的整棵进程树(含自身), 用于把 agent 全部子进程加入 eBPF 监控。
// 实现: 先构建一次 ppid→children 索引(O(N)), 再 BFS 遍历(O(N))。
func descendantPids(root int) []int {
	// 构建 ppid→children 索引
	children := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return []int{root}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cp, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid := readPpid(cp)
		children[ppid] = append(children[ppid], cp)
	}
	// BFS 从 root 开始遍历
	out := []int{root}
	for i := 0; i < len(out); i++ {
		out = append(out, children[out[i]]...)
	}
	return out
}

// ---- /proc 辅助 ----

// isShell 判断进程名是否为 shell(启动器), 不应作为 agent 吸收祖先
func isShell(comm string) bool {
	switch comm {
	case "bash", "sh", "zsh", "dash", "fish", "ksh", "tcsh", "csh":
		return true
	}
	return false
}

var toolReCache struct {
	mu sync.RWMutex
	m  map[string]*regexp.Regexp
}

func matchTool(cmdline, comm string, match []string) string {
	lower := strings.ToLower(cmdline)
	toolReCache.mu.RLock()
	cached := toolReCache.m
	toolReCache.mu.RUnlock()

	for _, kw := range match {
		if kw == "hermes" {
			// 精确匹配 Hermes Agent: 仅当 comm 为 hermes, 或 cmdline 含 hermes_cli 包路径
			// (如 `python -m hermes_cli.main gateway run`) 时视为 hermes 主 agent。
			// 避免 Hermes 平台基础设施(cua-driver 浏览器驱动 / hermes-snap 命令快照脚本)因
			// cmdline 路径含 "hermes" 目录名被误判成第二个主 agent(页面访问测试时出现双 hermes 的根因)。
			if strings.EqualFold(comm, "hermes") || strings.Contains(lower, "hermes_cli") {
				return kw
			}
			continue
		}
		// 其他工具: 编译或复用含 \b 边界的正则, 精确匹配工具名(避免 copilot 命中"copilot-child"等子进程名)
		re, ok := cached[kw]
		if !ok {
			re = regexp.MustCompile(`\b` + regexp.QuoteMeta(kw) + `\b`)
			toolReCache.mu.Lock()
			if toolReCache.m == nil {
				toolReCache.m = make(map[string]*regexp.Regexp)
			}
			toolReCache.m[kw] = re
			toolReCache.mu.Unlock()
		}
		if re.MatchString(lower) {
			return kw
		}
	}
	return ""
}

func findPts(pid int) string {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, "/dev/pts/") {
			return link
		}
	}
	return ""
}

// hasActiveChild 判断 pid 是否有存活的直接子进程(用于 running 判定的辅助信号)。
func hasActiveChild(pid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cpid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if cpid == pid {
			continue
		}
		if readPpid(cpid) == pid {
			return true
		}
	}
	return false
}

func readPpid(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return -1
	}
	s := string(data)
	idx := strings.Index(s, ")")
	if idx < 0 {
		return -1
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 2 {
		return -1
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

func readCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}

// readComm 读取 /proc/<pid>/comm(进程名, 不含参数)。
func readComm(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ansiRe 匹配 ANSI CSI 转义序列, 如 \x1b[32m / \x1b[?25h 等
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[0-9A-Za-z]")

// cleanLine 剥除 ANSI 转义与控制字符, 仅保留可见文本(含多字节 UTF-8 中文)。
// 用于 eBPF/pty 抓到的原始 pty 流(agent 常用控制序列刷新进度条/光标)。
func cleanLine(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '	' {
			b = append(b, c)
			continue
		}
		// 丢弃控制字符(<0x20 除 \n	) 与 DEL(0x7f); 多字节 UTF-8 字节 >=0x80 保留
		if c < 0x20 || c == 0x7f {
			continue
		}
		b = append(b, c)
	}
	return strings.TrimRight(string(b), "\r\n ")
}

// processExists 判断 pid 是否仍存在(用于异常检测时对已退出进程跳过, 避免对已死 pid 误报卡死)。
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); err != nil {
		return false
	}
	return true
}
