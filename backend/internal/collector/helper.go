package collector

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// safeStr 安全取 interface{} 的字符串值, 非字符串时返回 ""。
func safeStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// subStateOf 由 transcript 记录类型推断子代理状态(仅 assistant 消息内的 tool_use 块会进入,
// 此时外层 record 的 type 恒为 "message" -> 走 default 归 running; 历史 result 行不产生子代理)。
func subStateOf(recType string) string {
	switch recType {
	case "assistant", "tool_use":
		return "thinking"
	default:
		return "running"
	}
}

func resolveLLMHosts(hosts []string) map[string]bool {
	out := make(map[string]bool)
	for _, h := range hosts {
		// 关键字(如 "copilot"/"codex")无法解析, 作为子串匹配保留原样
		if !strings.Contains(h, ".") {
			out[h] = true
			continue
		}
		ips, err := net.LookupIP(h)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				out[v4.String()] = true
			}
		}
	}
	return out
}

// isAgentInternalPath 判断完整文件路径是否落在某 AI agent 自身的内部目录中
// (如 ~/.claude/、~/.codex/、~/.hermes/、~/.copilot/、~/.aider/)。这些目录下的文件是
// agent 的运行态/状态/缓存/会话, 绝非用户源码, 不应展示为"正在编辑"或"最近文件"。
// 传入的是完整路径(EvOpenat/EvRename 的 rawArg), 故可直接按目录判定, 比仅 basename 更精确。
func isAgentInternalPath(p string) bool {
	for _, dir := range []string{
		"/.claude/", "/.codex/", "/.hermes/", "/.copilot/", "/.aider/",
		"/.openclaw/", "/.gemini/", "/.config/claude/", "/.config/codex/",
	} {
		if strings.Contains(p, dir) {
			return true
		}
	}
	return false
}

// isKernelPseudoPath 判断完整路径是否落在内核伪文件系统(/proc、/sys、/dev)下。
// 这些不是用户文件: agent 平台(hermes / agent-scope 自身)会持续 openat("/proc/<pid>/stat")
// 读子进程元数据、读 /sys / /dev/pts 等, 这是内部进程管理行为, 绝非"正在编辑的文件"。
// 若不排除, 这些内部读会污染"文件"列(如 hermes 主节点 last_file 始终为 "stat",
// task 始终为 "处理 stat")。仅排除前缀即可覆盖 /proc/<pid>/stat 等真实 pathname;
// 另对裸 basename 命中已知 /proc 伪文件名(stat/status/cmdline/...)做兜底, 防止 eBPF
// 偶发投递短路径时漏判。
func isKernelPseudoPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/proc/") || strings.HasPrefix(p, "/sys/") ||
		strings.HasPrefix(p, "/dev/") {
		return true
	}
	switch base := filepath.Base(p); base {
	case "stat", "status", "cmdline", "maps", "smaps", "wchan", "environ",
		"io", "fd", "comm", "cgroup", "limits", "mountinfo", "mounts", "net",
		"attr", "auxv", "cpuset", "coredump_filter", "exe", "fdinfo", "gid_map",
		"loginuid", "mem", "mountstats", "ns", "numa_maps", "oom_adj", "oom_score",
		"oom_score_adj", "pagemap", "personality", "projid_map", "root", "sched",
		"schedstat", "sessionid", "setgroups", "stack", "statm", "syscall", "uid_map",
		"latency":
		return true
	}
	return false
}

// isTransientFile 判断文件名是否为代理内部临时/状态文件(不算用户编辑)。
// 命中: UUID 前缀(8 位十六进制 + -)、.tmp 后缀、纯随机十六进制名、.lock 等。
func isTransientFile(name string) bool {
	if name == "" {
		return false
	}
	// 已知临时/内部后缀
	if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".lock") ||
		strings.HasSuffix(name, ".swp") || strings.HasSuffix(name, ".pyc") ||
		strings.HasSuffix(name, ".swx") || strings.HasSuffix(name, ".bak") {
		return true
	}
	// SQLite 临时文件: .db-shm(共享内存)/.db-wal(预写日志)/.db-journal(回滚日志)是数据库
	// 内部文件, 绝非用户源码编辑。若不排除, agent 持续写 SQLite(如 hermes 的 kanban 库)会被
	// 误判为"编辑中"且 task 显示陈旧库文件名(如 '编辑 kanban.db-shm'), 掩盖真实工作状态。
	if strings.HasSuffix(name, ".db-shm") || strings.HasSuffix(name, ".db-wal") ||
		strings.HasSuffix(name, ".db-journal") {
		return true
	}
	// 只读共享库 / 本地化翻译文件: .mo(gettext locale)、.so(共享库)、.so.<ver>(带版本共享库)。
	// 这些是系统/运行时文件, agent 仅加载(读)绝不会"编辑", 若展示为"处理 xxx.mo/xxx.so"
	// 会掩盖真实工作状态(如 hermes 执行 coreutils 命令时加载 /usr/share/locale/.../coreutils.mo)。
	// 用户源码绝不会是 .mo/.so, 故零风险排除。
	if strings.HasSuffix(name, ".mo") || strings.HasSuffix(name, ".so") {
		return true
	}
	// 随机后缀临时文件: 形如 <base>.tmp.<随机> 或 <base>.<8hex> (如 hermes-snap-xxx.sh.tmp.MDv9CkeNf9, exp-cache.json.tmp.1e5cb45d)
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		suf := name[dot+1:]
		// 后缀本身是随机串(全字母数字且长度>=6, 无明显扩展名语义) -> 临时
		if len(suf) >= 6 && isAlnum(suf) {
			return true
		}
	}
	// 含典型 agent 内部命名片段
	for _, frag := range []string{"hermes-snap-", "exp-cache", ".tmp.", "-f506-", "drain_request", "resume-", ".claude-cache", "copilot-cache", ".agent-tmp"} {
		if strings.Contains(name, frag) {
			return true
		}
	}
	// UUID 前缀: 8 位十六进制 + '-'(如 105ebccf-f506-4a1e-...)
	if len(name) >= 9 && name[8] == '-' {
		hex := name[:8]
		ok := true
		for i := 0; i < 8; i++ {
			c := hex[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// isAlnum 判断字符串是否全为字母数字(用于识别随机后缀)
func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}
func isKnownLLMAgent(tool string) bool {
	switch strings.ToLower(tool) {
	case "copilot", "codex", "claude", "aider", "opencode", "gemini", "cursor", "agent":
		return true
	}
	return false
}

// isOutboundTLS 判断连接是否为到公网的外连 TLS(443)/80。
// 用于 LLM 连接兜底: 代理工具的外连公网 443 大概率是连 LLM。
func isOutboundTLS(conn string) bool {
	i := strings.LastIndexByte(conn, ':')
	if i < 0 {
		return false
	}
	port := conn[i+1:]
	if port != "443" && port != "80" {
		return false
	}
	ip := conn[:i]
	// 排除私有/回环/链路本地地址
	if ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "10.") ||
		strings.HasPrefix(ip, "192.168.") || strings.HasPrefix(ip, "169.254.") ||
		(strings.HasPrefix(ip, "172.") && isPrivate172(ip)) {
		return false
	}
	return true
}

func isPrivate172(ip string) bool {
	// 172.16.0.0/12
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return false
	}
	if parts[0] != "172" {
		return false
	}
	b, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return b >= 16 && b <= 31
}

// 解决黑盒代理(如 copilot)复用/持久 LLM 连接导致 sys_enter_connect 只触发一次、
// 之后 eBPF 抓不到连接事件的问题: 直接扫 /proc/<pid>/net/tcp 的远端 IP。
func hasLLMConn(pid int, llmIPs map[string]bool) bool {
	if pid <= 0 {
		return false
	}
	return hasLLMConnInFile(fmt.Sprintf("/proc/%d/net/tcp", pid), llmIPs)
}

// hasLLMConnInFile 解析 /proc/net/tcp 格式文件(供测试注入临时文件)。
// 第3列远端地址格式为 HEXIP:HEXPORT(小端), 第4列 01=ESTABLISHED。
func hasLLMConnInFile(path string, llmIPs map[string]bool) bool {
	if len(llmIPs) == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // 跳过表头
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "01" { // 仅 ESTABLISHED
			continue
		}
		remote := fields[2]
		colon := strings.IndexByte(remote, ':')
		if colon < 0 {
			continue
		}
		if ip := hexToIPv4(remote[:colon]); ip != "" {
			if llmIPs[ip] {
				return true
			}
		}
	}
	return false
}

// hexToIPv4 将 /proc/net/tcp 的小端十六进制 IP(如 6E0CA68C)转为点分十进制。
func hexToIPv4(hexIP string) string {
	if len(hexIP) != 8 {
		return ""
	}
	b, err := strconv.ParseUint(hexIP, 16, 32)
	if err != nil {
		return ""
	}
	v := uint32(b)
	return fmt.Sprintf("%d.%d.%d.%d", v&0xff, (v>>8)&0xff, (v>>16)&0xff, (v>>24)&0xff)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// execNameOf 从命令行取可执行名(argv[0] 的 basename)。
// 用于在"不泄露任何参数"的前提下定位是哪个进程出的问题 —— 参数里可能夹带凭据,
// 而可执行名本身是元数据, 可安全写入告警/日志/webhook。
func execNameOf(cmdLine string) string {
	s := strings.TrimSpace(cmdLine)
	if s == "" {
		return "?"
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	if b := filepath.Base(s); b != "" && b != "." && b != "/" {
		return b
	}
	return "?"
}

// scrubSecrets 掩掉命令行中敏感键的取值, 保留命令结构可读。
// 用于"既要展示命令、又不能外发凭据"的场合(破坏性命令告警: 用户必须看清删的是
// /tmp/x 还是 /etc, 但同一条命令行可能夹带 --password=xxx)。
//
// 对配置里每个凭据模式(password= / token= / AKIA / Bearer  / ...):
// 保留模式本身(它来自配置, 不是密钥), 把紧随其后直到下一个空白的内容替换为 ***。
// 大小写不敏感匹配; 无命中时原样返回。
func scrubSecrets(s string, patterns []string) string {
	for _, p := range patterns {
		if p != "" {
			s = scrubAfter(s, p)
		}
	}
	return s
}

// scrubAfter 掩掉 s 中每一处 pattern 之后的取值(到下一个空白为止)。
func scrubAfter(s, pattern string) string {
	const mask = "***"
	var b strings.Builder
	lowP := strings.ToLower(pattern)
	for {
		i := strings.Index(strings.ToLower(s), lowP)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(pattern)
		b.WriteString(s[:end]) // 模式本身保留
		rest := s[end:]
		j := strings.IndexAny(rest, " \t\n\r")
		if j < 0 {
			j = len(rest)
		}
		if j > 0 {
			b.WriteString(mask)
		}
		s = rest[j:]
	}
}

func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// claudeProjectHash 将 claude 会话所在 cwd 编码为 project hash。
// claude 将会话存在 ~/.claude/projects/<hash>/, 其中 hash = cwd 的所有 "/" 替换为 "-"
// (含开头的 "/"), 例如 /data/docker/compose/agent-scope -> -data-docker-compose-agent-scope。
// 零侵入: 纯字符串变换, 不读任何外部状态。
func claudeProjectHash(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}
