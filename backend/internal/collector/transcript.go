package collector

import (
	"agentmon/internal/config"
	"agentmon/internal/store"
	"encoding/json"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func extractSubagents(data []byte) []subagentInfo {
	var out []subagentInfo
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		msg, _ := rec["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		content, _ := msg["content"].([]interface{})
		for _, blk := range content {
			b, _ := blk.(map[string]interface{})
			if b == nil || b["type"] != "tool_use" {
				continue
			}
			name, _ := b["name"].(string)
			if !strings.Contains(strings.ToLower(name), "task") && !strings.Contains(strings.ToLower(name), "subagent") {
				continue
			}
			inp, _ := b["input"].(map[string]interface{})
			desc := ""
			if d, ok := inp["description"].(string); ok {
				desc = d
			} else if d, ok := inp["prompt"].(string); ok {
				desc = d
			}
			if desc == "" {
				continue
			}
			// tool_use 块的唯一 id: claude 每条 tool_use 必含 id 字段, 用作子 agent 的稳定唯一键,
			// 避免\"同名 task 描述\"碰撞(否则合成 pid 相同, 后写覆盖前写)。
			id, _ := b["id"].(string)
			out = append(out, subagentInfo{Name: name, Task: desc, ToolUseID: id, State: subStateOf(safeStr(rec["type"]))})
		}
	}
	return out
}

// mergeTranscriptSubagents 解析 Claude/Codex 的 transcript JSONL, 提取同会话子代理(Subagent/Task 调用),
// 作为根节点的子节点写入 store(即使子代理是进程内上下文, 无独立进程, 也能在树中呈现)。
// 这是"进程树"层看不到同会话子代理的补充(调研确认 Claude 子代理是 own-context 进程内, 非独立进程)。
// 修复: 用 subagentScanOffset 增量读新增行, 避免每轮全量读大文件。
func (c *Collector) mergeTranscriptSubagents(m *agentMonitor, t transcriptInfo, agentPID int, now time.Time, activePids map[int]bool) {
	f, err := os.Open(t.file)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	fileSize := fi.Size()
	if fileSize <= m.subagentScanOffset {
		return
	}
	if _, err := f.Seek(m.subagentScanOffset, io.SeekStart); err != nil {
		return
	}
	buf := make([]byte, fileSize-m.subagentScanOffset)
	if _, err := io.ReadFull(f, buf); err != nil {
		return
	}
	m.subagentScanOffset = fileSize

	rootPID := agentPID
	for _, sa := range extractSubagents(buf) {
		h := fnv.New64a()
		// 用 tool_use_id 作为唯一键(同文件下每条 tool_use 的 id 唯一); 防御: 极旧格式无 id 时回退 description。
		// 修复: 与 transcriptPID() 保持一致的 fnv64a, 降低合成 pid 碰撞概率。
		key := sa.ToolUseID
		if key == "" {
			key = sa.Task
		}
		h.Write([]byte(t.file + "|" + key))
		synthetic := int(h.Sum64()&0x7fffffffffffffff)%2000000000 + 1
		synthetic = -synthetic // 负数, 避免与真实 pid 冲突
		c.store.Upsert(store.Agent{
			PID: synthetic, Tool: "subagent:" + sa.Name, State: store.AgentState(sa.State),
			Confidence: "medium", LastText: sa.Task, UpdatedAt: now.Unix(),
			ParentPID: rootPID, RootPID: rootPID, Depth: 1, IsSubagent: true,
			Task: sa.Task, Src: "transcript",
		})
		activePids[synthetic] = true
	}
}

// transcriptPID 根据 transcript 文件路径合成一个稳定的负整数 PID。
// 先取 fnv64a 哈希的低 63 位(确保 int64 可正表示), 再取负 → 结果必定为负, 避免
// uint64 > MaxInt64 时 int(...) 绕回负值再取负得到正数的 bug。
func transcriptPID(file string) int {
	h := fnv.New64a()
	h.Write([]byte(file))
	return -int(h.Sum64() & 0x7fffffffffffffff)
}

// ---- transcript 扫描 ----

// transcriptInfo 是从一个 transcript(JSONL)会话文件解析出的状态快照。
type transcriptInfo struct {
	file     string
	tool     string
	lastTs   int64
	lastLine string
	realPID  int // 关联的真实 claude 进程 PID(0=进程已退出, 用合成 PID 回退)
	// 解析自最后一条 JSONL 的状态字段(供 transcriptState 推导权威状态)
	lastType         string // user / assistant / system / ...
	lastApiStatus    string // assistant 行的 apiErrorStatus(如 429/520)
	lastApiErr       string // assistant 行的 error(如 rate_limit)
	lastApiErrMsg    bool   // isApiErrorMessage
	lastApiTs        int64  // 末条 error 行的真实时间戳(用于判断 error 新鲜度, 而非末内容行 age)
	lastStopReason   string // assistant 行的 message.stop_reason(如 "tool_use")
	lastToolName     string // assistant 末次 tool_use 的工具名(仅显式交互工具可推导等待)
	pendingToolName  string // 尚无 tool_result 的工具名(有多个时优先显式交互工具)
	pendingToolCount int    // 尚无 tool_result 的 tool_use 数量
	lastNeedsInput   bool   // 推导: 是否在等待用户输入(approve tool_use / 用户回复)
}

// activeClaudeProjects 返回当前服务器上存活 claude 进程的映射:
// project hash → 真实 PID。用于将 session JSONL 文件关联到真实进程,
// 避免用户看到纯合成负 PID 时困惑"为什么不是真实进程号"。
// 同一 project hash 可能对应多个 claude 进程(同一 cwd 多次启动), 但
// 实际场景极少; 若多个, 取最新启动的 PID(lastPid 返回较大的那个)。
func activeClaudeProjects() map[string]int {
	set := map[string]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return set
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pidStr := e.Name()
		if _, err := strconv.Atoi(pidStr); err != nil {
			continue
		}
		commB, err := os.ReadFile(filepath.Join("/proc", pidStr, "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(commB)) != "claude" {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", pidStr, "cwd"))
		if err != nil {
			continue
		}
		hash := claudeProjectHash(cwd)
		pid, _ := strconv.Atoi(pidStr)
		// 同一 hash 可能存在多 claude 进程(罕见), 取较大 PID=后启动的
		if old, ok := set[hash]; !ok || pid > old {
			set[hash] = pid
		}
	}
	return set
}

func scanTranscripts(cfg *config.Config, monitors map[string]*agentMonitor) []transcriptInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []transcriptInfo
	// 当前活 claude 进程所在 project 集合: project hash → real PID。
	// 仅这些 project 的会话对应"实际运行的 agent", 其余历史归档会话不展示。
	activeProjects := activeClaudeProjects()
	claudeDir := filepath.Join(home, ".claude", "projects")
	if entries, err := os.ReadDir(claudeDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			realPID, isActive := activeProjects[e.Name()]
			// 仅处理"有活 claude 进程在其 cwd 对应 project"的目录; 历史归档 project 不展示。
			if !isActive {
				continue
			}
			projDir := filepath.Join(claudeDir, e.Name())
			fs, _ := os.ReadDir(projDir)
			// 活跃 project 下展示所有"近期活跃"的会话文件, 支持同一 cwd 下并发多个 claude 会话
			// (各自写不同 session 文件, 原逻辑只取 mtime 最新一个会漏采其余并发会话)。
			// 时间窗过滤: 仅 mtime 在 recentClaudeSessionWindow 内的会话才展示, 历史归档会话
			// (早已退出、文件不再写入)自然超出窗口 -> 不展示, 与"进程退出即消失"语义一致;
			// 窗口外的陈旧文件另由 readTranscript 的 2h 丢弃提供二层保护。
			const recentClaudeSessionWindow = 7200 // 2 小时 (匹配 readTranscript 的 2h 丢弃时间)
			// 注意: activeClaudeProjects() 已确保仅"有活 claude 进程"的 project 被扫描,
			// 因此该窗口只是辅助过滤, 防止已退出进程的陈旧会话残留。2h 足够。
			nowUnix := time.Now().Unix()
			for _, sf := range fs {
				if sf.IsDir() || !strings.HasSuffix(sf.Name(), ".jsonl") {
					continue
				}
				fi, err := sf.Info()
				if err != nil {
					continue
				}
				if nowUnix-fi.ModTime().Unix() > recentClaudeSessionWindow {
					continue
				}
				tf := filepath.Join(projDir, sf.Name())
				// 该 project 有活 claude 进程(cwd 匹配)且会话近期活跃 -> 作为 agent 节点展示,
				// 状态由 readTranscript 从会话末行权威推导(空闲/推理/错误/等待)。历史归档 project
				// 已在上方 activeProjects 过滤时排除。这是 v1.8.31/v1.8.33 修复 + v1.8.36 并发多会话支持。
				if info := readTranscript(tf, "claude", monitors); info != nil {
					info.realPID = realPID
					out = append(out, *info)
				}
			}
		}
	}
	codexDir := filepath.Join(home, ".codex", "sessions")
	if entries, err := os.ReadDir(codexDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			tf := filepath.Join(codexDir, e.Name())
			if info := readTranscript(tf, "codex", monitors); info != nil {
				out = append(out, *info)
			}
		}
	}
	return out
}

// detectTranscriptVersion 探测转录本格式版本(只读首行, 开销可忽略)。
// claude/codex 当前格式(v1)特征: 含 type 字段, 且 assistant 行带 message.stop_reason /
// isApiErrorMessage 等字段。未来大版本变更(字段重排)将由此处识别并返回 v2+, 触发分派。
// 识别失败时返回 "v1"(最安全兜底, 不中断监控)。
func detectTranscriptVersion(path, tool string) string {
	f, err := os.Open(path)
	if err != nil {
		return "v1"
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "v1"
	}
	// 扫描前几行找首个有效 JSON, 检查是否含 claude/codex v1 特征字段。
	for _, ln := range strings.Split(string(buf[:n]), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var probe struct {
			Type    string `json:"type"`
			Message struct {
				StopReason string `json:"stop_reason"`
			} `json:"message"`
			IsApiErrorMessage bool `json:"isApiErrorMessage"`
		}
		if err := json.Unmarshal([]byte(ln), &probe); err != nil {
			continue
		}
		if probe.Type != "" {
			// 当前 claude/codex transcript 均为 v1 格式。
			return "v1"
		}
		break
	}
	return "v1"
}

func readTranscript(path, tool string, monitors map[string]*agentMonitor) *transcriptInfo {
	key := "transcript:" + path
	m := monitors[key]

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	fileSize := fi.Size()

	// 转录本版本路由: 不同 agent / 同 agent 不同大版本(如 claude 改 transcript schema)的
	// JSONL 格式可能不同。先探测版本, 再分派到对应解析器, 避免"宽松兜底"在大版本变更时
	// 静默误判。当前仅实现 v1(claude/codex 当前格式, 见下方解析逻辑); 未来新版本在此
	// 加 case + 独立解析函数, 不保留旧版兼容 shim(干净断代)。
	// 探测在解析前完成(只读首行), 开销可忽略。
	ver := detectTranscriptVersion(path, tool)
	_ = ver // 当前 v1 解析器兼容所有已识别版本; 新版本在此分派。

	// 确定偏移量: 新 monitor 从 0 开始, 已有 monitor 从上一次位置继续
	offset := int64(0)
	if m != nil {
		offset = m.transcriptOffset
	}
	if offset > fileSize {
		offset = 0 // 文件被截断/重建, 重头读
	}

	// 跳过已读部分
	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return nil
		}
	}

	// 读取新增内容
	remaining := fileSize - offset
	if remaining == 0 {
		// 文件无新增: claude 处于空闲(未在写入)。此时不能简单返回 nil —— 否则该轮 txs 为空,
		// 会让上层(scanTranscripts/状态推导)丢失这个会话, 造成节点在 idle 时闪烁甚至退化回
		// proc 误判 running。改用 monitor 缓存的"上次末行状态"继续代表该会话(状态稳定为 idle)。
		if m != nil && m.lastLine != "" {
			return &transcriptInfo{
				file: path, tool: tool, lastTs: m.lastOut.Load(),
				lastLine: m.lastLine, lastType: m.lastType,
				lastApiStatus: m.lastApiStatus, lastApiErr: m.lastApiErr, lastApiErrMsg: m.lastApiErrMsg,
				lastApiTs:      m.lastApiTs,
				lastStopReason: m.lastStopReason, lastToolName: m.lastToolName,
				pendingToolName:  pendingToolName(m.pendingTools),
				pendingToolCount: len(m.pendingTools),
				lastNeedsInput:   m.lastNeedsInput,
			}
		}
		return nil // 首次扫描且文件确实为空
	}

	buf := make([]byte, remaining)
	if _, err := f.Read(buf); err != nil {
		return nil
	}

	// 更新 offset 到文件末尾(下次只读新增)
	if m != nil {
		m.transcriptOffset = fileSize
	}

	// 如果新增字节全部是空白, 跳过
	if strings.TrimSpace(string(buf)) == "" {
		return nil
	}

	// 取最后一行(JSONL: 每行一个 JSON 对象)
	allLines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	lastLine := strings.TrimSpace(allLines[len(allLines)-1])
	if lastLine == "" {
		return nil
	}

	// 遍历本次新增的所有行(不只最后一行): claude 遇到 429 时会写入含 apiErrorStatus/error
	// 的 assistant 行, 但该行通常不是文件末尾(随后重试/继续写正常行), 只取最后一行会漏掉 429
	// 信号 -> 页面始终空闲。这里扫描整个 chunk 捕获错误, 并在遇到正常 assistant 完成行时清除。
	type msgStop struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	type recT struct {
		Type              string      `json:"type"`
		Timestamp         string      `json:"timestamp"`
		ApiErrorStatus    interface{} `json:"apiErrorStatus"`
		Error             interface{} `json:"error"`
		IsApiErrorMessage bool        `json:"isApiErrorMessage"`
		Message           *msgStop    `json:"message,omitempty"`
	}
	var (
		lastType        string
		lastTs          int64
		lastApiTs       int64  // 末条 error 行的真实时间戳(扫描时累积)
		lastContentLine string // 末条"非跳过"内容行原始 JSON(用于 last_text 展示, 避免抓到 attachment 噪声)
		errStatus       string
		errMsg          string
		lastStopReason  string
		lastToolName    string
	)
	pendingTools := make(map[string]string)
	if m != nil && offset > 0 {
		for id, name := range m.pendingTools {
			pendingTools[id] = name
		}
	}
	for _, ln := range allLines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var r recT
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			continue
		}
		// 跳过 claude 内部元数据行: 这些行(如 mode/permission-mode/atis-latch/last-prompt/
		// file-history-snapshot/queue-operation) 是 claude 进程内部簿记, 无空时戳或仅有
		// 操作记录, 出现在用户输入后、assistant 输出前, 或会话结束时。跳过它们可使
		// lastType 保留有意义的类型 (user/assistant/system), 避免状态误判为 idle。
		switch r.Type {
		case "last-prompt", "mode", "permission-mode", "atis-latch", "file-history-snapshot", "queue-operation":
			continue
		}
		// attachment 有 ts 但只是文件引用记录, 跳过以保留前面的 user 类型。
		if r.Type == "attachment" {
			continue
		}
		lastType = r.Type
		lastContentLine = ln
		lastTs = parseTs(r.Timestamp)
		if r.Message != nil {
			for _, content := range r.Message.Content {
				switch content.Type {
				case "tool_use":
					if content.ID != "" {
						pendingTools[content.ID] = content.Name
					}
				case "tool_result":
					delete(pendingTools, content.ToolUseID)
				}
			}
		}
		if r.ApiErrorStatus != nil || (r.Error != nil && r.IsApiErrorMessage) {
			switch v := r.ApiErrorStatus.(type) {
			case string:
				errStatus = v
			case float64:
				errStatus = strconv.Itoa(int(v))
			}
			if s, ok := r.Error.(string); ok {
				errMsg = s
			}
			// 记录该 error 行的真实时间戳, 供 transcriptState 判断 error 新鲜度
			// (基准必须是 error 行自身时间, 而非末内容行 age —— 否则陈旧 520 会永久覆盖真实的等待输入态)。
			lastApiTs = parseTs(r.Timestamp)
		} else if r.Type == "assistant" {
			// 正常 assistant 行(无错误信号) -> 已恢复/正常推理, 清除错误标记
			errStatus = ""
			errMsg = ""
			// 解析 stop_reason: assistant 以 tool_use 结束时, 在等用户确认
			if r.Message != nil && r.Message.StopReason == "tool_use" {
				lastStopReason = "tool_use"
				lastToolName = ""
				for i := len(r.Message.Content) - 1; i >= 0; i-- {
					if r.Message.Content[i].Type == "tool_use" {
						lastToolName = r.Message.Content[i].Name
						break
					}
				}
			} else {
				lastStopReason = ""
				lastToolName = ""
			}
		} else if r.Type == "user" {
			// 新的一轮对话开始: 上一轮的 stop_reason (如 tool_use) 不适用于新一轮,
			// 清除防止从上一层残留导致误判为 "waiting(等待用户确认)"。
			lastStopReason = ""
			lastToolName = ""
		}
	}

	// 如果该 chunk 中所有行都被跳过(仅含 metadata/attachment, 无实际内容), 则
	// lastType/lastTs 保持零值。此时若有 monitor 缓存, 复用缓存态(状态不变); 否则
	// 返回 nil 让该节点在本周期短暂消失(后续有新内容会重新出现)。
	if lastType == "" {
		if m != nil && m.lastType != "" {
			return &transcriptInfo{
				file: path, tool: tool, lastTs: m.lastOut.Load(),
				lastLine: m.lastLine, lastType: m.lastType,
				lastApiStatus: m.lastApiStatus, lastApiErr: m.lastApiErr, lastApiErrMsg: m.lastApiErrMsg,
				lastApiTs:      m.lastApiTs,
				lastStopReason: m.lastStopReason, lastToolName: m.lastToolName,
				pendingToolName:  pendingToolName(m.pendingTools),
				pendingToolCount: len(m.pendingTools),
				lastNeedsInput:   m.lastNeedsInput,
			}
		}
		return nil
	}

	// last-prompt 是 claude 写往 session 文件的内部归档标记, 表示该会话已结束(不再写入)。
	// 已结束的会话不应作为独立 agent 展示, 否则用户会看到两个 claude 节点(一个 idle 旧会话
	// + 一个当前活跃会话), 造成困惑。过滤掉以 last-prompt 结尾的 session 文件。
	if lastType == "last-prompt" {
		return nil
	}

	ts := lastTs
	// 放宽时间窗: 仅丢弃极陈旧(>2h)的会话; 近期的空闲会话由 transcriptState 按 age 判定为 idle。
	if time.Now().UnixNano()-ts > int64(7200)*int64(time.Second) {
		return nil
	}
	ti := &transcriptInfo{file: path, tool: tool, lastTs: ts, lastLine: lastContentLine}
	ti.lastType = lastType
	ti.lastApiStatus = errStatus
	ti.lastApiErr = errMsg
	ti.lastApiErrMsg = errStatus != "" || errMsg != ""
	ti.lastApiTs = lastApiTs
	ti.lastStopReason = lastStopReason
	ti.lastToolName = lastToolName
	ti.pendingToolName = pendingToolName(pendingTools)
	ti.pendingToolCount = len(pendingTools)
	// lastNeedsInput 不再由 transcript 内容推断(stop_reason==tool_use / user 都会误标活跃会话为
	// 等待输入)。claude 的 needs_input 以 proc 的 pty 探测为权威(transcript 无法区分"执行工具"与
	// "卡等 approve")。故这里固定 false, 避免活跃 claude 被陈旧 transcript 误判等待输入。
	ti.lastNeedsInput = false
	// 缓存到 monitor, 供 claude 空闲(文件无新增)时继续代表该会话, 避免节点闪烁/退化。
	if m != nil {
		m.lastLine = lastContentLine
		m.lastType = lastType
		m.lastApiStatus = errStatus
		m.lastApiErr = errMsg
		m.lastApiErrMsg = ti.lastApiErrMsg
		m.lastApiTs = lastApiTs
		m.lastStopReason = lastStopReason
		m.lastToolName = lastToolName
		m.lastNeedsInput = false
		m.pendingTools = pendingTools
	}
	return ti
}

func pendingToolName(tools map[string]string) string {
	for _, name := range tools {
		if claudeInteractionReason(name) != "" {
			return name
		}
	}
	for _, name := range tools {
		return name
	}
	return ""
}

// transcriptState 由 transcript 最后一条 JSONL 的真实字段推导 claude/codex 的权威状态。
// 零侵入: 仅读 JSONL 文件(与 copilot 读 events.jsonl 同源思路), 不控制 agent。
//   - assistant 且含 apiErrorStatus/error -> error(如 429 rate_limit), 归入异常
//   - assistant, 显式交互 tool_use -> waiting(等待用户回答)
//   - assistant, 普通 tool_use -> thinking(工具正在调度或执行, 不能等同于等待批准)
//   - assistant(无错误, 非 tool_use) -> thinking(调用 LLM / 推理中)
//   - user -> thinking(处理中: 用户已输入, 等待 agent 响应)
//   - system(turn_duration 等)或距末活动超 idleNs -> idle(空闲)
//     注意: Claude transcript 没有通用的 permission-requested 事件。普通 tool_use 既可能马上
//     执行，也可能触发权限 UI，不能仅凭 stop_reason 猜测等待确认。
func transcriptState(t transcriptInfo, idleNs int64) (state, reason, errorCode string) {
	now := time.Now().UnixNano()
	age := now - t.lastTs
	errAge := now - t.lastApiTs // error 行自身的年龄(而非末内容行 age)
	// 错误信号优先, 但其新鲜度基准必须是 error 行自身时间(errAge), 而非末内容行 age:
	// 否则陈旧 520(发生在用户重发消息之前)会永久覆盖真实的\"等待用户输入\"态。
	// 只有 error 行本身是近期(<=idleNs)发生的才显示 error; 陈旧 error 不显示, 退化到后续分支
	// (user->thinking_user 等)。恢复防护仍由 readTranscript 保证: 正常 assistant 行会清除 errStatus。
	// 错误码(如 "429"/"520")单独用 errorCode 字段传递, 由前端本地化渲染(避免后端硬编码中文)。
	if (t.lastApiStatus != "" || (t.lastApiErr != "" && t.lastApiErrMsg)) && errAge <= idleNs {
		st := t.lastApiStatus
		if st == "" {
			st = t.lastApiErr
		}
		return "error", store.ReasonLLMError, st
	}
	// 只有协议中明确要求用户交互的工具才能确定为等待。Bash/Read/Edit 等普通 tool_use
	// 在工具执行期间同样是 transcript 末行，将它们判为等待会造成持续误报。
	if t.lastType == "assistant" && t.lastStopReason == "tool_use" {
		if reason := claudeInteractionReason(t.lastToolName); reason != "" {
			return "waiting", reason, ""
		}
	}

	if age > idleNs {
		// 超时且无 tool_use 等待: 对话空闲 / 生成完等下一句 / 内部簿记 -> 退化 idle。
		// 但若 claude 进程仍然存活(realPID>0)且末行是 user/assistant(正在进行工作),
		// 使用更长的进程存活窗口(5min)而非 idleNs(60s):
		// LLM 调用(1-3分钟)期间 claude 不写 transcript, 短 idleNs 会导致"页面显示空闲但实际在推理"。
		// 进程退出后(realPID==0)才正常退化到 idle。
		if t.realPID > 0 && (t.lastType == "user" || t.lastType == "assistant") {
			const processAliveNs = 300 * 1e9 // 5 分钟: 超过此时间无任何 transcript 写入 + 进程存活 -> 真空闲
			if age > processAliveNs {
				return "idle", store.ReasonIdle, ""
			}
			if t.lastType == "assistant" {
				return "thinking", store.ReasonThinkingLLM, ""
			}
			return "thinking", store.ReasonThinkingUser, ""
		}
		return "idle", store.ReasonIdle, ""
	}
	// 活跃(age 小): assistant/user 是高活跃信号 -> 处理中(thinking), 不再用"等待 agent 响应"歧义。
	if t.lastType == "assistant" {
		return "thinking", store.ReasonThinkingLLM, ""
	}
	if t.lastType == "user" {
		return "thinking", store.ReasonThinkingUser, ""
	}
	// system / 未知内部类型(file-history-snapshot / atis-latch 等): claude 内部簿记, 视为空闲。
	return "idle", store.ReasonIdle, ""
}

func claudeInteractionReason(name string) string {
	switch strings.ToLower(name) {
	case "askuserquestion", "ask_user":
		return store.ReasonAwaitingInput
	case "exitplanmode", "exit_plan_mode":
		return store.ReasonAwaitingApproval
	default:
		return ""
	}
}

func parseTs(s string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixNano()
	}
	if ep, err := strconv.ParseInt(s, 10, 64); err == nil {
		if ep > 1e12 {
			return ep * int64(time.Millisecond)
		}
		return ep * int64(time.Second)
	}
	return time.Now().UnixNano()
}
