package collector

import (
	"agentmon/internal/config"
	"agentmon/internal/ebpf"
	"agentmon/internal/notify"
	"agentmon/internal/store"
	"agentmon/internal/wss"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Collector struct {
	cfg      *config.Config
	store    *store.Store
	notifier *notify.Notifier
	hub      *wss.Hub
	ebpfMon  *ebpf.Monitor
	mu       sync.Mutex
	monitors map[string]*agentMonitor
	// pidOwner: 任意被 eBPF 监控的 pid(含子进程) -> 其所属的 canonical monitor 的 pid
	pidOwner map[int]int
	// llmIPs: 已解析的 LLM API IP 集合(由 Behavior.LLMHosts 解析), 用于 connect 事件匹配
	llmIPs map[string]bool
	// 告警冷却: (pid,kind) -> 上次记录 unix 秒, 避免每轮 scan 重复写库
	alertMu   sync.Mutex
	lastAlert map[string]int64
	// txs: 最近一次扫描得到的各 agent 转录本信息(权威状态源)。
	// 按 tool 名索引(claude/codex 各自持转录本), 替代原 claude 专用字段 claudeTx。
	// 通用兜底: 未注册 Source 的 agent(aider/gemini 等)无转录本, 该 map 不含其项。
	txs map[string]*transcriptInfo
}

func New(cfg *config.Config, st *store.Store, nt *notify.Notifier, hub *wss.Hub, ebpfMon *ebpf.Monitor) *Collector {
	c := &Collector{cfg: cfg, store: st, notifier: nt, hub: hub, ebpfMon: ebpfMon,
		monitors: make(map[string]*agentMonitor), pidOwner: make(map[int]int),
		llmIPs: resolveLLMHosts(cfg.Behavior.LLMHosts), lastAlert: make(map[string]int64),
		txs: make(map[string]*transcriptInfo)}
	// 注册各 agent 的状态推导 Source(Strategy 模式): 新增 agent = 加一个 Source 文件 + 此注册一行,
	// 主循环只通过 SourceOf(tool) 接口交互, 无 if tool== 硬分支。
	Register(claudeSource{})
	Register(codexSource{})
	Register(copilotSource{})
	Register(hermesSource{})
	return c
}

func (c *Collector) Run(ctx context.Context) {
	// 快速树刷新: 持续把 agent 进程树的子进程加入 eBPF 监控, 避免子进程在 scan 间隔内
	// 已生灭导致 execve/openat/connect 漏采(timing gap)。
	treeTicker := time.NewTicker(500 * time.Millisecond)
	scanTicker := time.NewTicker(time.Duration(c.cfg.Collect.Interval) * time.Second)
	defer treeTicker.Stop()
	defer scanTicker.Stop()
	c.refreshTrees()
	c.scan()
	for {
		select {
		case <-ctx.Done():
			c.shutdown()
			return
		case <-treeTicker.C:
			c.refreshTrees()
		case <-scanTicker.C:
			c.scan()
		}
	}
}

// scan 是采集主循环的一轮: 拉取 eBPF 行为事件 -> 扫描转录本 -> 构建 /proc 树 -> 识别根 agent
// 及其后代并推导状态写库 -> 写入 transcript 会话节点 -> 清理消失的 monitor 与过期数据 ->
// 通过 WebSocket 广播快照。
func (c *Collector) scan() {
	c.pollBehavior() // 先拉取 eBPF 行为事件, 注入到对应 monitor
	seen := make(map[string]bool)
	// 本轮活跃 agent 的 pid 集合(proc 真实 pid / transcript 合成 tpid / subagent 合成 pid)。
	// 用于告警生命周期绑定: 状态型告警若绑定 pid 不在本集合(agent 已退出/会话归档),
	// 则在 scan 末尾自动解除, 避免"进程早退出了告警还挂 7 天"。
	activePids := make(map[int]bool)

	// 0) 先扫描 transcript(JSONL) 会话文件: claude/codex 的权威状态源。
	// 提前扫描, 使下方 proc 分支能为对应 agent 主 agent 采用 transcript 推导的准确状态。
	txs := scanTranscripts(c.cfg, c.monitors)
	// 注意: 不在此将 c.txs 置空。claude/codex 会持续向 jsonl 写入, 偶尔某一轮 scanTranscripts
	// 读到半行(写入中)导致 readTranscript 返回 nil、该轮 txs 为空。若每轮都重置, 则空轮会让
	// c.txs 变空 -> proc 节点退化回误判 running。保留上一轮成功解析的值, 保证抑制稳定。
	// hasTxThisRound: 本轮 scanTranscripts 是否有某 tool 转录本。用于下方 proc 分支条件:
	// 仅当"本轮解析出某 tool 转录本"时才跳过对应 proc root, 避免 c.txs 保留旧值但转录本已超
	// 10 分钟窗口导致 agent 在页面上完全消失(proc 被跳过, transcript 也不展示)。
	hasTxThisRound := map[string]bool{}
	for i := range txs {
		tool := txs[i].tool
		hasTxThisRound[tool] = true
		cur, ok := c.txs[tool]
		if !ok || txs[i].lastTs > cur.lastTs {
			c.txs[tool] = &txs[i]
		}
	}

	// 1) 构建整棵 /proc 树
	tree := buildProcTree()

	// 2) 识别根 agent: cmdline 命中 match 且祖先未命中(避免子进程被当成独立 root)且不在排除列表
	matchedRoots := make(map[int]bool)
	for pid, n := range tree {
		tool := matchTool(n.cmdline, n.comm, c.cfg.Collect.Match)
		if tool == "" {
			continue
		}
		// 祖先若已命中 match -> 通常是某 root 的后代(如 copilot 的 MainThread 子进程)。
		// 但若祖先命中的是"不同工具"(如从 hermes 终端启动的 copilot), 则它们是独立 agent,
		// 不应被吸收进祖先的工具树 -> 仅当祖先与当前进程命中同一工具时才视为后代。
		// 此外: 跳过 shell 祖先(bash/sh/zsh/dash) —— 即使其命令行参数恰含某工具名(如用户
		// 从名为 copilot.sh 的脚本启动), shell 本身不是 agent, 不应作为吸收祖先。
		anc := n.ppid
		isDesc := false
		myTool := matchTool(n.cmdline, n.comm, c.cfg.Collect.Match)
		for anc != 0 && anc != 1 {
			an, ok := tree[anc]
			if !ok {
				break
			}
			if isShell(an.comm) {
				anc = an.ppid
				continue
			}
			if at := matchTool(an.cmdline, an.comm, c.cfg.Collect.Match); at != "" {
				if at == myTool {
					isDesc = true
				}
				break // 命中任一 match 祖先即停止上溯(避免跨工具误吸收)
			}
			anc = an.ppid
		}
		if isDesc {
			continue
		}
		if isExcluded(tree, pid, c.cfg.Collect.Exclude) {
			continue
		}
		matchedRoots[pid] = true
	}

	// 3) 为每个 root 及其后代建立 monitor + 计算状态 + 写库(树结构)
	// 预构建 ppid→children 索引, 避免 BFS 中 O(N²) 遍历整棵树
	childrenOf := make(map[int][]int)
	for pid, n := range tree {
		childrenOf[n.ppid] = append(childrenOf[n.ppid], pid)
	}
	now := time.Now()
	foldedParent := map[int]int{} // 被折叠节点的 pid -> 其应归属的 copilot rootPid(用于子进程归并)
	for rootPid := range matchedRoots {
		// BFS 收集后代(含自身), 用 (pid, depth) 对保证深度正确
		type nodeDepth struct{ pid, depth int }
		desc := []nodeDepth{{rootPid, 0}}
		for i := 0; i < len(desc); i++ {
			for _, child := range childrenOf[desc[i].pid] {
				// 排除项(自身或祖先命中排除列表的进程)不显示为节点, 也不展开其子树。
				// 否则本项目自身进程(agent-scope)作为宿主 agent(如 hermes)的子进程被吸收进树。
				if isExcluded(tree, child, c.cfg.Collect.Exclude) {
					continue
				}
				desc = append(desc, nodeDepth{child, desc[i].depth + 1})
			}
		}
		for _, nd := range desc {
			depth, pid := nd.depth, nd.pid
			n := tree[pid]
			// 折叠 copilot 私有执行体(MainThread 等): 不单独显示为节点, 但其 spawn 的
			// 工具子进程(bash 等)仍处理, 并把它们的父指针归并到 copilot 主节点。
			if depth > 0 && isCopilotPrivateExec(n) {
				foldedParent[pid] = rootPid
				continue
			}
			// 折叠 Hermes 基础设施占位执行体(cua-driver 空转 node 及其 bash 宿主壳):
			// 不单独显示为节点, 但其 spawn 的真实工具子进程(若 computer_use 干活时 fork)仍处理,
			// 父指针归并到 hermes 主节点。
			if depth > 0 && isHermesInfraExec(n) {
				foldedParent[pid] = rootPid
				continue
			}
			tool := n.comm // 默认用进程名(可读: node/MainThread/script/bash...)
			if depth == 0 {
				// 根: 用 match 命中的工具名(claude/copilot/...)
				if mt := matchTool(n.cmdline, n.comm, c.cfg.Collect.Match); mt != "" {
					tool = mt
				}
			} else if mt := matchTool(n.cmdline, n.comm, c.cfg.Collect.Match); mt != "" && !strings.Contains(n.cmdline, "copilot") {
				// 子进程: 仅当命令名明显是某 agent 时才用匹配名, 否则用 comm
				tool = mt
			}
			key := "proc:" + strconv.Itoa(pid)
			seen[key] = true
			c.mu.Lock()
			m, ok := c.monitors[key]
			c.mu.Unlock()
			if !ok {
				m = c.startProcMonitor(key, pid, tool)
				if m != nil {
					c.mu.Lock()
					c.monitors[key] = m
					c.mu.Unlock()
				}
			}
			if m == nil {
				continue
			}
			parentPID := 0
			if depth > 0 {
				// 直接父(若父是被折叠的 MainThread, 则归并到 copilot 主节点)
				parentPID = n.ppid
				if rp, ok := foldedParent[parentPID]; ok {
					parentPID = rp
				}
			}
			m.hasChild.Store(hasActiveChild(pid))
			if c.ebpfMon != nil {
				for _, dp := range descendantPids(pid) {
					c.ebpfMon.AddPID(dp)
					c.pidOwner[dp] = rootPid
				}
				if ts, ok := c.ebpfMon.LastActive(pid); ok {
					m.lastOut.Store(ts)
					m.ebpfUsed = true
				}
			}
			// 有 transcript 权威根节点的 agent(claude/codex): 其 proc root 节点跳过,
			// 由下方 transcript 节点展示(更准确, 避免内部文件读写误判 running)。
			// 但子进程(bash/node/git 等真实 OS 子进程)仍需处理并挂入进程树, 不能跳过整棵子树。
			// 使用 hasTxThisRound[tool] 而非 c.txs[tool]!=nil, 防止转录本超 10 分钟窗口后
			// 旧值导致 proc 和 transcript 两分支都跳过, 进程完全消失在页面上。
			// transcript 根仍需先探测终端事件循环；它会与未完成 tool_use 组合识别真实授权等待。
			blocked := m.probePts()
			m.ptsBlocked.Store(blocked)
			if src, ok := SourceOf(tool); ok && src.HasTranscriptRoot() && hasTxThisRound[tool] && depth == 0 {
				continue
			}
			stStr, cf, needsInput, cmd, file, conn, reason := m.updateState(
				c.cfg.Collect.IdleSeconds, c.cfg.Collect.WaitInputSeconds, c.llmIPs)
			st := store.AgentState(stStr)
			task := "" // 由下方 Source 或默认 taskOf 填充
			// 经 AgentSource 接口推导状态(消除 if tool== 硬分支):
			// 各 agent(claude/copilot/hermes/codex)实现自己的 Source 并注册到 registry,
			// 主循环只通过接口交互。未注册 tool(aider/gemini 等)走通用 updateState 兜底。
			if src, ok := SourceOf(tool); ok {
				r := src.DeriveProcState(m, c.txs[tool], 60*int64(time.Second))
				if r.State != "" {
					st, reason, needsInput = r.State, r.Reason, r.NeedsInput
					cf = "high"
					if r.Task != "" {
						task = r.Task
					}
				}
			}
			c.store.Upsert(store.Agent{
				PID: pid, Tool: tool, State: st, Confidence: cf,
				LastText: m.lastText(), UpdatedAt: now.Unix(),
				LastCmd: cmd, LastFile: copilotFileOr(file, tool, pid), LastConn: conn,
				StateReason: reason, StateErrorCode: "",
				NeedsInput: needsInput,
				ParentPID:  parentPID,
				RootPID:    rootPid,
				Depth:      depth,
				IsSubagent: depth > 0,
				Task:       taskOr(task, taskOf(tool, file, cmd, "")),
				Src:        "proc",
			})
			activePids[pid] = true
			if needsInput {
				m.recordEvents(c.store, pid, cmd, file, conn, "needs_input")
			}
			// 异常检测只在 root agent(depth==0) 上跑: 子进程(脚本/shell 壳)的等待输入
			// 本质是父 agent 会话在等输入, 不应每个子节点独立刷告警(树重构后避免 N 倍刷屏)。
			if depth == 0 {
				c.detectAnomalies(m, pid, string(st), needsInput, m.lastText())
			}
		}
	}

	// 4) transcript(JSONL) 根节点(Claude/Codex 会话)。transcripts 已在 scan 开头扫描(见步骤0)。
	//
	// 合并策略: 同一真实 claude 进程(realPID>0)下的多个 session 文件合并为一个 agent 节点,
	// 使用真实 PID 展示(用户看到熟悉的进程号而非负哈希)。多 session 取最新(lastTs 最大)的权威状态。
	// 真实进程退出后(realPID=0)各 session 退化为各自独立节点, 用合成 PID 保持历史可追溯。
	// 先按 realPID 分组(仅 claude 有 realPID; codex 始终 realPID=0 按文件独立)。
	mergedByReal := make(map[int]transcriptInfo) // realPID→最新 transcript
	mergedFrom := make(map[int][]string)         // realPID→session 文件列表
	for _, t := range txs {
		pid := t.realPID
		if pid <= 0 { // 进程退出或 codex: 不归组, 保持逐文件独立
			continue
		}
		existing, ok := mergedByReal[pid]
		if !ok || t.lastTs > existing.lastTs {
			mergedByReal[pid] = t
		}
		mergedFrom[pid] = append(mergedFrom[pid], t.file)
	}
	// 对已归并的 realPID 跳过逐文件循环; 未归并的 session(realPID=0)正常遍历。
	seenReal := make(map[int]bool) // 已写过的 realPID, 跳过同 pid 的后续 session 文件
	for _, t := range txs {
		pid := t.realPID
		key := "transcript:" + t.file
		seen[key] = true
		if pid > 0 && seenReal[pid] {
			continue // 同 realPID 的后续 session 文件已合并到第一个节点, 但 monitor 仍需 seen 标记
		}
		if pid > 0 {
			seenReal[pid] = true
		}
		// 使用真实的agent PID: realPID>0用真实PID, 否则用合成PID回退
		agentPID := t.realPID
		if agentPID == 0 {
			agentPID = transcriptPID(t.file)
		}
		// 构建session信息(同realPID合并时列出所有session文件)
		task := t.file
		if pid > 0 {
			files := mergedFrom[pid]
			if len(files) > 1 {
				sessionDetail := strings.Join(files, "; ")
				if len(sessionDetail) > 200 {
					sessionDetail = sessionDetail[:200] + "..."
				}
				if t.tool == "claude" {
					task = "sessions: " + sessionDetail
				}
			}
		}
		// 多sessions合并时使用最新的transcriptInfo作为权威
		tx := t
		if pid > 0 {
			if merged, ok := mergedByReal[pid]; ok {
				tx = merged
			}
		}
		c.mu.Lock()
		m, ok := c.monitors[key]
		c.mu.Unlock()
		if !ok {
			m = &agentMonitor{key: key, tool: tx.tool, src: "transcript", ringCap: c.cfg.Collect.PtyRing}
			c.mu.Lock()
			c.monitors[key] = m
			c.mu.Unlock()
		}
		m.tool = tx.tool
		// 当 transcript 文件无新增(返回缓存数据), lastTs 仍为旧的 JSONL 时间戳。
		// LLM 推理(1-3 分钟)期间 claude 不写 transcript, 导致 age 持续增长超 idleNs 而退化 idle。
		// 若对应 proc monitor 近期有 eBPF 活动(LLM 连接/pty 输出/文件操作等), 它们证明
		// claude 进程存话且正活跃, 用系统时间 now 刷新 lastTs, 使进程存活的 claude 在
		// 推理期间保持 thinking 态, 不会因 transcript 静默而"显示空闲"。
		if pid > 0 {
			if pm, ok := c.monitors["proc:"+strconv.Itoa(agentPID)]; ok {
				if pm.lastOut.Load() > tx.lastTs+int64(30*time.Second) {
					tx.lastTs = time.Now().UnixNano()
				}
			}
		}
		m.lastOut.Store(tx.lastTs)
		m.mu.Lock()
		m.lines = []string{tx.lastLine}
		m.mu.Unlock()
		// claude/codex 主 agent 的状态以 transcript 最后一行权威推导(空闲/推理/错误/等待),
		// 不再用 proc 内核推断(会被内部文件读写误判 running), 与 copilot 走 events.jsonl 同源。
		// 经 AgentSource 接口推导(消除 if tool== 硬分支): 各 transcript-based agent 实现
		// DeriveTranscriptState; 未注册 tool 退化为通用 transcriptState 兜底。
		var txState string
		var txReason string
		var txErrCode string
		var txNeeds bool
		var txConfidence string
		// 取同 PID 的终端事件循环阻塞信号。该信号仅与未完成 tool_use 组合使用，
		// 避免把同样阻塞于 epoll 的 LLM 推理误判为等待输入。
		inputPollBlocked := false
		if pm, ok := c.monitors["proc:"+strconv.Itoa(agentPID)]; ok {
			inputPollBlocked = pm.inputPollBlocked.Load() && !pm.hasChild.Load()
		}
		if src, ok := SourceOf(tx.tool); ok {
			r := src.DeriveTranscriptState(&tx, 60*int64(time.Second), inputPollBlocked)
			txState, txReason, txErrCode, txNeeds = string(r.State), r.Reason, r.ErrorCode, r.NeedsInput
			txConfidence = r.Confidence
		} else {
			s, rs, ec := transcriptState(tx, 60*int64(time.Second))
			txState, txReason, txErrCode = s, rs, ec
			txNeeds = s == "waiting"
		}
		st := store.AgentState(txState)
		cf := "high"
		if txConfidence != "" {
			cf = txConfidence
		} else if st == store.StateIdle {
			cf = "low"
		}
		// 告警用 agentPID(同 realPID 合并时所有 sessions 共享告警状态)
		if st == store.StateError {
			c.fireAlert(agentPID, m.tool, "llm_error", "critical",
				fmt.Sprintf("LLM接口错误(transcript捕获): %s", txErrCode))
		} else {
			c.store.DeleteAlertsKind(agentPID, "llm_error")
		}
		needsInput := txNeeds || tx.lastNeedsInput
		c.store.Upsert(store.Agent{
			PID: agentPID, Tool: m.tool, State: st, Confidence: cf,
			LastText: m.lastText(), UpdatedAt: now.Unix(),
			LastCmd: "", LastFile: "", LastConn: "", StateReason: txReason, StateErrorCode: txErrCode,
			NeedsInput: needsInput,
			ParentPID:  0, RootPID: agentPID, Depth: 0, IsSubagent: false,
			Task: task, Src: "transcript",
		})
		activePids[agentPID] = true
		m.recordEvents(c.store, agentPID, "", "", "", string(st))
		// 解析同会话子代理并作为子节点写入
		c.mergeTranscriptSubagents(m, tx, agentPID, now, activePids)
	}

	// 5) 清理消失的 monitor
	c.mu.Lock()
	for key, m := range c.monitors {
		if !seen[key] {
			if c.ebpfMon != nil && strings.HasPrefix(key, "proc:") {
				if pid, err := strconv.Atoi(strings.TrimPrefix(key, "proc:")); err == nil {
					c.ebpfMon.DelPID(pid)
				}
			}
			if m.cancel != nil {
				m.cancel()
			}
			delete(c.monitors, key)
		}
	}
	c.mu.Unlock()

	c.store.Prune(now.Add(-time.Duration(c.cfg.Collect.Interval*2) * time.Second).Unix())
	// 行为时间线: 按 store.retain_events_sec 保留(默认 60s)。0 表示不清理。
	if c.cfg.Store.RetainEventsSec > 0 {
		c.store.PruneEvents(now.Add(-time.Duration(c.cfg.Store.RetainEventsSec) * time.Second).Unix())
	}
	// 异常告警: 按 store.retain_alerts_days 保留(默认 7 天)。0 表示不清理。
	if c.cfg.Store.RetainAlertsDays > 0 {
		c.store.PruneAlerts(now.Add(-time.Duration(c.cfg.Store.RetainAlertsDays) * 24 * 60 * 60 * time.Second).Unix())
	}
	// 告警生命周期绑定: 状态型告警(llm_error/stuck/wait_unhandled)若绑定 pid 已不在本轮活跃
	// agent 集合(进程退出 / 会话归档), 自动解除。避免"agent 早退出了告警还挂 retention 天数"。
	// 安全审计类(secret_leak/destructive_cmd)为一次性事件记录, 保留以追溯(零丢失)。
	c.clearOrphanStateAlerts(activePids)

	// WebSocket 实时推送: 每轮 scan 后广播一次树快照(若有人订阅)
	if c.hub != nil && c.hub.Len() > 0 {
		if agents, err := c.store.ListTree(); err == nil {
			alerts, _ := c.store.RecentAlerts(50)
			c.hub.Push(wss.Msg{
				Type:   "tree",
				Agents: mustJSON(agents),
				Alerts: mustJSON(alerts),
				Now:    now.Unix(),
			})
		}
	}
}

// taskOf 生成该节点的任务摘要(用于子 agent 展示)。
func taskOf(tool, file, cmd, detail string) string {
	if detail != "" {
		return detail
	}
	if file != "" {
		return "处理 " + file
	}
	if cmd != "" {
		return "执行 " + cmd
	}
	return ""

}
