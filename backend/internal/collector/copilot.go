package collector

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agentmon/internal/store"
)

// subagentInfo 是解析出的同会话子代理(Claude Code Task/Subagent 调用)。
type subagentInfo struct {
	Name      string // 工具名, 如 Task / Subagent
	Task      string // 任务描述(description/prompt)
	ToolUseID string // tool_use 块的唯一 id(claude 每条 tool_use 必有), 用作子 agent 稳定唯一键
	State     string // 子代理状态(running/thinking, 由 subStateOf 推断)
}

// parseCopilotState 以 copilot 自身写入的 events.jsonl 为权威状态来源(解决"用户随时可输入" vs
// "需要用户输入才能继续" 的区分难题: 内核态两者都是阻塞 tty, 无法靠 /proc/eBPF 区分;
// 而 copilot 在 ask_user / permission.requested 时明确写进 events.jsonl, 才是真 waiting)。
// 返回 (state, task, file, needsInput):
//   - state: waiting/running/thinking/idle
//   - task: 给人看的任务摘要(含工具名+文件/命令上下文)
//   - file: 当前正在操作的文件路径(或 bash 命令摘要), 供前端"文件"列显示(优于陈旧的 eBPF last_file)
//   - needsInput: 是否真的在等用户决策才能继续
//
// 修复: 不再硬编码 /root, 改用 os.UserHomeDir() 检测运行用户的真实 home 目录。
// copilot 默认安装在 ~/.copilot/, 其 session 存储在 ~/.copilot/session-state/。
func parseCopilotState() (string, string, string, bool) {
	r := parseCopilotStateResult()
	return r.state, r.task, r.file, r.needsInput
}

type copilotStateResult struct {
	state      string
	reason     string
	task       string
	file       string
	needsInput bool
}

func parseCopilotStateResult() copilotStateResult {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}
	dir := filepath.Join(home, ".copilot", "session-state")
	return parseCopilotStateResultInDirForCWD(dir, "")
}

func parseCopilotStateResultForPID(pid int) copilotStateResult {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}
	cwd, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	return parseCopilotStateResultInDirForCWD(filepath.Join(home, ".copilot", "session-state"), cwd)
}

// parseCopilotStateInDir 同 parseCopilotState, 但 session 目录可配置(便于单测注入临时目录)。
func parseCopilotStateInDir(dir string) (string, string, string, bool) {
	r := parseCopilotStateResultInDir(dir)
	return r.state, r.task, r.file, r.needsInput
}

// 修复(v1.8.37): copilot 1.0.80 用专属事件表达"等待用户输入/确认",
// 而旧逻辑只认 permission.requested 且只读尾部 64KB, 导致 copilot 处于
// "用户输入确认状态"时页面误显 idle。本次修复:
//   - 识别全部等待事件(user_input/elicitation/tool_user/exit_plan_mode/permission 的 .requested)
//   - 放大尾部窗口到 256KB, 并跟踪"最近一次请求事件"是否已被 .completed 解除
//   - session 选择排除已 session.shutdown 的目录(选真正活跃的最新会话)
func parseCopilotStateResultInDir(dir string) copilotStateResult {
	return parseCopilotStateResultInDirForCWD(dir, "")
}

func parseCopilotStateResultInDirForCWD(dir, wantedCWD string) copilotStateResult {
	// 定位 session: 取 mtime 最新且未关闭(session 末事件非 shutdown)的 events.jsonl。
	// 避免选到已退出的历史会话(其 mtime 可能仍很新), 与"进程退出即消失"语义一致。
	entries, err := os.ReadDir(dir)
	if err != nil {
		return copilotStateResult{}
	}
	type cand struct {
		path string
		mod  int64
		cwd  string
	}
	var cands []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "events.jsonl")
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		cands = append(cands, cand{path: p, mod: fi.ModTime().UnixNano(), cwd: copilotSessionCWD(p)})
	}
	if wantedCWD != "" {
		var matching []cand
		for _, c := range cands {
			if c.cwd == wantedCWD {
				matching = append(matching, c)
			}
		}
		if len(matching) > 0 {
			cands = matching
		}
	}

	// 按 mtime 降序
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod > cands[j].mod })
	// 逐个尝试: 选第一个末事件非 session.shutdown 的(跳过已关闭会话)
	var latest string
	for _, c := range cands {
		if lastEventIsShutdown(c.path) {
			continue
		}
		latest = c.path
		break
	}
	if latest == "" {
		// 全部是已关闭会话 -> 退而求其次取 mtime 最新者
		if len(cands) > 0 {
			latest = cands[0].path
		} else {
			return copilotStateResult{}
		}
	}
	// 扫尾部(避免大文件全量读)。256KB 覆盖更长会话的近期活跃状态,
	// 足以捕获"最近一次 user_input/permission 请求"事件(即便其后 copilot 又做了若干工具调用)。
	const maxTail int64 = 256 * 1024 // 256KB ≈ 1200-2500 行 events.jsonl
	f, err := os.Open(latest)
	if err != nil {
		return copilotStateResult{}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return copilotStateResult{}
	}
	tailSize := fi.Size()
	if tailSize > maxTail {
		tailSize = maxTail
		if _, err := f.Seek(-maxTail, io.SeekEnd); err != nil {
			return copilotStateResult{}
		}
	}
	buf := make([]byte, tailSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return copilotStateResult{}
	}
	// 丢弃第一行可能被截断的部分(从第一行完整的 \n 后开始)
	content := buf
	if tailSize >= maxTail {
		if idx := bytes.IndexByte(buf, '\n'); idx >= 0 {
			content = buf[idx+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	// 跟踪工具调用: 记录"最近一次"未完成的工具调用(toolCallId), 以及权限等待态。
	type toolCall struct {
		name string
		args string
		file string
	}
	incomplete := map[string]toolCall{} // toolCallId -> 调用详情
	var toolOrder []string              // 启动顺序，用于选择最新的未完成并行工具
	var lastToolFile string
	inTurn := false // 是否处于某轮 assistant 对话中(turn_start 后、turn_end 前)
	finalAnswer := false
	// 等待用户输入/确认态: 跟踪"最近一次出现的等待请求事件"及其是否已被解除。
	// copilot 1.0.80 表达等待的事件(user_input/elicitation/tool_user/exit_plan_mode/permission
	// 的 .requested)可能不在文件末尾(其后 copilot 可能又做了工具调用), 故用"最近一次请求"
	// 模型而非"尾部第一个未完成工具"判定, 才能稳定捕获等待态。
	type waitRequest struct {
		reason string
		arg    string
		file   string
	}
	waitRequests := map[string]waitRequest{} // requestId -> 未完成等待请求
	var waitOrder []string                   // 请求顺序，用于展示最新请求
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		d, _ := rec["data"].(map[string]interface{})
		switch typ {
		case "tool.execution_start":
			tc, _ := d["toolCallId"].(string)
			name, _ := d["toolName"].(string)
			args := summarizeCopilotArgs(d["arguments"])
			file := copilotFileOf(d["arguments"]) // 提取操作文件/命令(用于前端"文件"列)
			if tc != "" {
				incomplete[tc] = toolCall{name: name, args: args, file: file}
				toolOrder = append(toolOrder, tc)
				lastToolFile = file
			}
		case "tool.execution_complete":
			tc, _ := d["toolCallId"].(string)
			if tc != "" {
				delete(incomplete, tc)
			}
		case "assistant.turn_start":
			// 新一轮对话 -> 之前的等待请求已被处理(用户已回应 / agent 已继续), 解除等待态
			inTurn = true
			finalAnswer = false
			clear(waitRequests)
			waitOrder = nil
		case "assistant.turn_end":
			// 一轮结束 -> 退出"分析中"状态。同时清除 finalAnswer, 避免 turn_end 后
			// 仍被 final_answer message 残留误判"thinking/总结回答"。空闲时应判 idle 而非 thinking。
			inTurn = false
			finalAnswer = false
		case "assistant.message":
			if ph, _ := d["phase"].(string); ph == "final_answer" {
				finalAnswer = true
			}
		case "session.shutdown":
			// 会话已关闭 -> 等待态解除
			clear(waitRequests)
			waitOrder = nil
		// —— 等待用户输入/确认事件(copilot 1.0.80) ——
		case "permission.requested", "user_input.requested", "elicitation.requested",
			"tool_user.requested", "exit_plan_mode.requested":
			requestID, _ := d["requestId"].(string)
			if requestID == "" {
				requestID = typ
			}
			reason, arg := copilotWaitDetail(typ, d, lastToolFile)
			waitRequests[requestID] = waitRequest{reason: reason, arg: arg}
			waitOrder = append(waitOrder, requestID)
		case "permission.completed", "user_input.completed", "elicitation.completed",
			"tool_user.completed", "exit_plan_mode.completed",
			"permission.resolved", "user_input.resolved", "elicitation.resolved",
			"tool_user.resolved", "exit_plan_mode.resolved",
			"permission.rejected", "exit_plan_mode.rejected":
			requestID, _ := d["requestId"].(string)
			if requestID != "" {
				delete(waitRequests, requestID)
			} else {
				// 旧事件格式没有 requestId，只能按事件族解除对应请求。
				prefix := strings.SplitN(typ, ".", 2)[0] + ".requested"
				delete(waitRequests, prefix)
			}
		}
	}
	// 1) 任一等待请求未完成 -> 真正等待用户输入/确认。并行请求必须逐 requestId 解除。
	for i := len(waitOrder) - 1; i >= 0; i-- {
		if req, ok := waitRequests[waitOrder[i]]; ok {
			return copilotStateResult{
				state: "waiting", reason: req.reason, task: req.arg, file: req.file, needsInput: true,
			}
		}
	}
	// 2) 选择最新的未完成工具；不能只检查最后启动的工具，否则并行工具先后完成时会漏报。
	for i := len(toolOrder) - 1; i >= 0; i-- {
		tc := toolOrder[i]
		call, ok := incomplete[tc]
		if !ok {
			continue
		}
		name, args, file := call.name, call.args, call.file
		if isCopilotWaitTool(name) {
			q := args
			if q == "" {
				q = name
			}
			return copilotStateResult{
				state: "waiting", reason: store.ReasonAwaitingInput, task: q, file: file, needsInput: true,
			}
		}
		task := name
		if file != "" {
			task = task + " " + file
		} else if args != "" {
			task = task + " " + args
		}
		return copilotStateResult{state: "running", task: task, file: file}
	}
	// 3) 无未完成的最近工具
	if finalAnswer {
		return copilotStateResult{state: "thinking", reason: store.ReasonThinkingLLM, file: lastToolFile}
	}
	if inTurn {
		return copilotStateResult{state: "thinking", reason: store.ReasonThinkingLLM, file: lastToolFile}
	}
	// 4) 空闲(一轮结束、无进行中工具) -> idle(等用户输入新 prompt), 不再误显 thinking/等待输入。
	return copilotStateResult{state: "idle", reason: store.ReasonIdle}
}

func copilotSessionCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 64*1024).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	var rec struct {
		Type string `json:"type"`
		Data struct {
			Context struct {
				CWD string `json:"cwd"`
			} `json:"context"`
		} `json:"data"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session.start" {
		return ""
	}
	return rec.Data.Context.CWD
}

// copilotWaitDetail 把 copilot 的等待请求事件翻译成前端可本地化的(原因枚举, 参数)。
// 不同事件语义: 权限确认 / 用户输入 / 补充信息(elicitation) / 用户操作工具 / 退出计划模式确认。
// 返回 (reason 枚举, arg): reason 供前端 i18n 渲染(避免后端硬编码中文),
// arg 为关联的工具名/文件(中性数据, 如 "Write" / "auth/login.py")。
func copilotWaitDetail(typ string, d map[string]interface{}, fallbackFile string) (string, string) {
	var file string
	if f := copilotFileOf(d); f != "" {
		file = f
	} else {
		file = fallbackFile
	}
	name, _ := d["toolName"].(string)
	if request, ok := d["permissionRequest"].(map[string]interface{}); ok {
		if name == "" {
			name, _ = request["kind"].(string)
		}
		if file == "" {
			if s, ok := request["fileName"].(string); ok {
				file = s
			}
		}
	}
	switch typ {
	case "permission.requested":
		if name != "" {
			return store.ReasonAwaitingApproval, name
		}
		return store.ReasonAwaitingApproval, file
	case "user_input.requested":
		return store.ReasonAwaitingInput, file
	case "elicitation.requested":
		return store.ReasonAwaitingInput, file
	case "tool_user.requested":
		if name != "" {
			return store.ReasonAwaitingApproval, name
		}
		return store.ReasonAwaitingInput, file
	case "exit_plan_mode.requested":
		return store.ReasonAwaitingApproval, file
	default:
		return store.ReasonAwaitingApproval, file
	}
}

// lastEventIsShutdown 判断 events.jsonl 的末(非空)事件是否为 session.shutdown。
// 用于 session 选择时排除已关闭的历史会话。
func lastEventIsShutdown(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	tail := int64(4 * 1024) // 末事件必在最后 4KB 内
	sz := fi.Size()
	if sz == 0 {
		return false
	}
	off := sz - tail
	if off < 0 {
		off = 0
		tail = sz
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, off); err != nil {
		return false
	}
	// 取最后一行(末事件)
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			return false
		}
		t, _ := rec["type"].(string)
		return t == "session.shutdown"
	}
	return false
}

// copilotFileOf 从 copilot 工具调用的 arguments 提取"正在操作的文件/命令"摘要,
// 用于前端"文件"列(优于陈旧的 eBPF last_file)。view/edit/create/apply_patch 取 path,
// bash 取 command 截断, ask_user 取问题首句。
func copilotFileOf(v interface{}) string {
	switch a := v.(type) {
	case string:
		// apply_patch 类: 提取 Add File:/Update File: 的文件名
		s := strings.TrimSpace(a)
		for _, pre := range []string{"Add File: ", "Update File: "} {
			if i := strings.Index(s, pre); i >= 0 {
				rest := s[i+len(pre):]
				if j := strings.IndexByte(rest, '\n'); j >= 0 {
					rest = rest[:j]
				}
				return strings.TrimSpace(rest)
			}
		}
		return ""
	case map[string]interface{}:
		// 优先文件类字段
		for _, k := range []string{"path", "file", "file_path"} {
			if s, ok := a[k].(string); ok && s != "" {
				return s
			}
		}
		// bash 的 command
		if s, ok := a["command"].(string); ok && s != "" {
			s = strings.TrimSpace(s)
			if len(s) > 60 {
				s = s[:60] + "…"
			}
			return s
		}
		// ask_user 的问题首句
		if s, ok := a["question"].(string); ok && s != "" {
			s = strings.TrimSpace(s)
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[:i]
			}
			if len(s) > 50 {
				s = s[:50] + "…"
			}
			return s
		}
	}
	return ""
}

// isCopilotPrivateExec 判断进程是否为 copilot 的私有执行体(原生二进制, 如 MainThread)。
// copilot 的 node 主进程会 spawn 出同名原生二进制(内核 comm 为 "MainThread", cmdline 含
// copilot-linux-x64/copilot), 它是 copilot 的内部实现细节(不对应独立 agent), 不应单独显示为
// 监控节点(其内核态 running/blocked 会误显"卡死/无响应", 而真正状态由主节点的 events.jsonl 表达)。
// 但它 spawn 的工具子进程(bash 等)仍有独立监控价值, 应保留并归并到 copilot 主节点下。
func isCopilotPrivateExec(n procNode) bool {
	if n.comm == "MainThread" {
		return true
	}
	if strings.Contains(n.cmdline, "copilot-linux-x64/copilot") ||
		strings.Contains(n.cmdline, "copilot-linux-aarch64/copilot") ||
		strings.Contains(n.cmdline, "copilot-darwin-arm64/copilot") ||
		strings.Contains(n.cmdline, "copilot-darwin-x64/copilot") {
		return true
	}
	return false
}

// isHermesInfraExec 判断进程是否为 Hermes 平台基础设施的"空转占位"执行体(非真实 agent 子任务),
// 不应单独显示为监控节点。典型为 Hermes computer_use / cua-driver 的后台驱动占位进程:
//
//	node -e process.stdin.resume(); setInterval(function(){}, 1000);
//
// 由 hermes gateway 为支持当前 session 的 computer_use 能力常驻(环境变量带 HERMES_SESSION_PLATFORM
// 等), 内核态阻塞等 stdin、每秒空转, 不做任何真实 agent 工作。若把它当 agent 子节点展示,
// 会误显为 "node blocked"(低置信度噪音), 与 copilot MainThread 陷阱同类。
// 注意: 不能一刀切排除所有 node —— hermes 可能正常运行 node 编写的 agent 子任务(需保留监控)。
// 故指纹必须精确命中"占位空转"特征: process.stdin.resume() + setInterval(...) 空转。
func isHermesInfraExec(n procNode) bool {
	lower := strings.ToLower(n.cmdline)
	switch strings.ToLower(n.comm) {
	case "node":
		// 占位 node: -e 内同时含 stdin.resume 与 setInterval 空转
		if strings.Contains(lower, "process.stdin.resume()") &&
			strings.Contains(lower, "setinterval(function(){}, 1000)") {
			return true
		}
	case "bash":
		// 其宿主壳: bash -lic set +m; node -e "process.stdin.resume(); setInterval(...)" ; echo "NODE_IDLE_DONE"
		if strings.Contains(lower, "node -e") &&
			strings.Contains(lower, "process.stdin.resume()") &&
			strings.Contains(lower, "setinterval") &&
			strings.Contains(lower, "node_idle_done") {
			return true
		}
	}
	return false
}

// isCopilotWaitTool 判断 copilot 工具是否为"需要用户输入才能继续"的等待型工具。
func isCopilotWaitTool(name string) bool {
	switch strings.ToLower(name) {
	case "ask_user", "ask-user", "askuser", "user_input", "user-input",
		"permission", "confirm", "approval", "input":
		return true
	}
	return false
}

// summarizeCopilotArgs 把 copilot 工具参数压缩成一行简短摘要(文件/命令/路径)。
func summarizeCopilotArgs(v interface{}) string {
	switch a := v.(type) {
	case string:
		s := strings.TrimSpace(a)
		if s == "" {
			return ""
		}
		// apply_patch 类: 提取 Add File:/Update File: 的文件名
		if strings.Contains(s, "Add File:") || strings.Contains(s, "Update File:") {
			for _, pre := range []string{"Add File: ", "Update File: "} {
				if i := strings.Index(s, pre); i >= 0 {
					rest := s[i+len(pre):]
					if j := strings.IndexByte(rest, '\n'); j >= 0 {
						rest = rest[:j]
					}
					return strings.TrimSpace(rest)
				}
			}
		}
		// 过长的纯文本截断
		if len(s) > 60 {
			return s[:60] + "…"
		}
		return s
	case map[string]interface{}:
		// view/run 等: 提取 path/command 字段
		for _, k := range []string{"path", "command", "file"} {
			if s, ok := a[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	return ""
}

// taskOr 优先用 Source 提供的显式任务摘要(explicit), 为空时回退默认推导(default)。
func taskOr(explicit, fallback string) string {
	if explicit != "" {
		return explicit
	}
	return fallback
}

// copilotFileOr 对 copilot 工具尝试从 events.jsonl 解析当前操作的文件/命令(优于陈旧的 eBPF last_file);
// 解析为空时返回空(不再回退 eBPF 的 last_file, 避免 copilot 启动期读 settings.json / 旧命令残留误导)。
func copilotFileOr(toDefault, tool string, pid int) string {
	if strings.EqualFold(tool, "copilot") {
		if f := parseCopilotStateResultForPID(pid).file; f != "" {
			return f
		}
		return "" // copilot 空闲/无明确文件时显式返回空, 不回退 eBPF 陈旧值
	}
	return toDefault
}

// 纯函数, 便于单测; 不依赖 store/collector。
