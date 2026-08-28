# WIP: eBPF 行为采集重写(完整重写 A 方案)

日期: 2026-08-21
状态: ✅ 完成并本地验证, 待提交+发版

## 目标
把采集层从"单点 pty write 刮屏"重写成"eBPF 直接采集 agent 进程树全部相关 syscall
(execve/openat/connect/write), 聚合成可读工作态(editing/thinking/running)", 并保留
隐私闸门(仅元数据, 不落盘文件内容/pty 字节)。

## 已完成
- BPF 源 `internal/ebpf/bpf/agent_mon.bpf.c` + 自包含 `bpf_types.h`(无 vmlinux.h, clang 独立编译)
  - 4 个 tracepoint: sys_enter_write / sys_enter_execve / sys_enter_openat / sys_enter_connect
  - 行为事件系统级采集写入 `beh_map`(HASH, key=pid, value=struct beh 96 字节)
  - on_write 仅作 running 信号(查 agent_pids), execve/openat/connect 系统级捕获
  - connect 解析 daddr:port; openat 记录文件名(逐字节读, Go 侧截断+取 basename)
- Go loader `internal/ebpf/ebpf.go`: 加载 collection, link.Tracepoint 挂 4 程序, PollEvents 读 beh_map
- 行为模型 `internal/collector/collector.go`:
  - refreshTrees 500ms 持续把 agent 进程树加入 agent_pids(解决子进程生灭 timing gap)
  - pollBehavior 按持久化 pidOwner 把事件路由到 canonical monitor
  - consumeEvent 处理事件, 更新 last_cmd/last_file/last_conn/lastEditFile
  - updateState 状态机: running/thinking/editing/waiting/idle/blocked, LLM host 解析为 IP 匹配
- store/config/index.html: Agent 加 LastCmd/LastFile/LastConn/StateDetail 字段; 前端渲染新徽章+字段
- 默认 Match 去掉 python(误报 irqbalance-ng), agent 关键词易误报 elkeid-agent, 真实部署无此噪声

## 关键技术坑(已解决)
1. `bpf_map_lookup_elem` 真实签名 3 参数(map,key,flags) — 自写声明漏 flags 会导致 helper 返回垃圾
2. Go `Event` struct 必须与 BPF `struct beh` 严格同构(96 字节), 否则 iter.Next 静默 0 条
3. `bpf_probe_read_user_str` 在本机失败 → 改用 `bpf_probe_read_user`(逐字节)+ Go 侧截断到首个 NUL + basename
4. ringbuf 在本机 "missing BTF" → 改用 HASH beh_map
5. 子进程在 scan 间隔内生灭 → 系统级捕获 + 持久化 pidOwner 路由解决

## WebSocket 实时推送(2026-08-22, v1.7.0)
目标: 消除 2s 轮询延迟, 状态/告警秒级到达(仍坚守"只观测、不控制"——WS 仅服务端推送, 客户端绝不发送任何控制指令)。
- 新增 wss 包(gorilla/websocket, 纯 Go 零 CGO): Hub 管理连接, 仅推送 snapshot(agents+alerts), 读循环只用于检测断线
- server: 新增 `GET /ws` 升级端点 + `snapshot()` 构造全量消息; New() 接收 hub
- collector: 持有 hub, 每轮 scan 后若有人订阅则广播 snapshot; 新增 mustJSON
- main: 创建 hub, 注入 collector + server
- 前端 App.vue: connectWS() 替代 2s 轮询(主通道), 断线递增退避重连; REST 兜底改为 10s 慢刷新; applySnapshot 复用状态变化高亮
- 单测: wss.TestHubPush(推送+断开移除); 真机验证: WS 客户端实测 open→首帧(3 agents)→每 ~2s 一帧; Playwright 确认 ws://.../ws 连接 + 卡片/告警面板渲染正常, 无 console 错误

## 进程树 / 子代理可视化(2026-08-22, v1.8.0)
用户核心诉求: 只看 root agent(copilot/claude/codex 等)及其**子 agent**的"在做什么任务 + 各自状态"。
现代 agent 普遍把任务派给多个子 agent(Claude Code 同会话 subagent / Codex / 后台会话 / agent team)。
彻底重构(不遗留历史债务): 删除扁平模型的所有补丁(findAgentProcs/seenProcTool/markToolChild/两层 desc 限制), 改为进程树 + 逻辑树融合。
- store: `agents` 表新增 `parent_pid / root_pid / depth / is_subagent / task / src` 列; `ListTree()` 返回全部节点扁平列表(每节点带 children pid 数组); 启动重建该表(无历史兼容)
- config: 新增 `collect.exclude`(排除列表: 自身或祖先 cmdline 命中则不当 agent); **修掉历史债务**——从 `match` 移除过宽的 `agent`/`node`(曾把 hermes/elkeid 误判成 agent)
- collector 进程树层: `buildProcTree()` 一次性扫全 /proc 建整棵树; root = match 命中且祖先未命中(避免子进程被当独立 root)且不在 exclude; 对每个后代 BFS 建 monitor、独立算状态、写树字段; 子进程 tool 用 comm(node/MainThread/script)更可读
- collector transcript 层: `mergeTranscriptSubagents()` 解析 Claude/Codex 的 transcript JSONL, 抓 `tool_use`(Task/subagent)提取子代理名+状态, 作为根的子节点写入(弥补同会话子代理无独立进程、纯进程树看不到的盲区; 调研确认 Claude 子代理是 own-context 进程内, 非独立进程)
- API/WS: `/api/agents` 与 WS 快照改为推树(`type:"tree"`); 前端按 parent_pid 组装
- 前端: 新增递归组件 `AgentNode.vue`, 树形渲染(root 卡片展开看子 agent); 子代理显示「任务/状态」徽标; 搜索/状态筛选做子树裁剪; WS 兼容 `tree`/`snapshot`
- 验证: Playwright 视觉确认 `copilot → script → node → MainThread` 四层树缩进渲染正常、**无 console 报错**; 单测全过(含 TestDetectAnomalies/TestHubPush/store)
- 约束坚守: 纯展示/只读 /proc + transcript, **绝不控制/干预** agent

## 异常检测 + 主动通知(2026-08-22, v1.6.0)
目标: 对 agent"需要输入或异常"时主动提醒(用户核心诉求)。
- config: 新增 `notify`(webhook_url/webhook_mention/system_notify/log_file/cooldown_seconds) + `alert`(stuck_seconds/wait_seconds/error_keywords)
- 新增 notify 包: 三种渠道 Webhook(飞书/钉钉/企微通用)/ 本地桌面(notify-send)/ 日志文件, 内置 (pid,kind) 冷却
- 新增 store `alerts` 表 + RecordAlert / RecentAlerts; API `/api/alerts`
- collector.detectAnomalies: 三类异常
  * stuck: 非活动态(idle/unknown/blocked)且超过 stuck_seconds 无输出 -> 卡死/无响应(critical)
  * wait_unhandled: 等待用户输入超过 wait_seconds 未处理 -> 警告
  * llm_error: 输出文本命中 error_keywords(429/timeout/panic/OOM...) -> 严重
  * 内置冷却, 同一 (pid,kind) 在 CooldownSeconds 内只写库一次(防每轮 scan 刷屏)
- 前端 App.vue: 红色严重告警条 + 异常告警面板 + 卡片异常横幅/红框
- 单测覆盖三类检测 + 冷却 + notifier 日志写入; 真机/真实数据验证: 向 DB 注入真实告警 -> API 返回 + Playwright 确认红条/面板/卡片横幅渲染正常

## 全面可观测 + 等待用户输入提醒(2026-08-22, v1.5.0)
用户授权放开隐私闸门, 目标: 对 agent 全面可观测 + 在"需要输入/异常"时主动提醒。
- config: Capture 默认改 `full`(完整 pty 输出 + 行为元数据); 新增 `collect.wait_input_seconds`(默认 8)
- collector: 只要 agent 连到 pty(交互终端)就启动 readLoop 全量读取输出文本(eBPF 仅给行为元数据)
- 新增 `probePts()`: FIONREAD 查 pty 待读字节 + 读 /proc wchan/stat 判断阻塞在终端输入
- `updateState` 重写:
  - 收紧 LLM 活跃判定: 仅"近期 connect"或"近期 eBPF 行为"才算 llmActive(解决复用连接误判 running)
  - 新增 needsInput: ptsBlocked + pty 无待读字节 + 安静 + 非 LLM 活跃 -> waiting(等待用户输入, 最高优先, 需提醒)
  - 输出文本命中确认词(Y/n等)也判 waiting
- store: agents 表加 `needs_input` 列; Agent 加 NeedsInput 字段; recordEvents 记录 needs_input
- 前端 App.vue: 顶部全局告警条 + 卡片"⚠ 等待你输入/确认"横幅 + 状态徽章

真机验证: 实机 copilot 阻塞在 pty 输入 -> 后端 state=waiting needs_input=True;
Playwright 确认全局告警 + 卡片横幅 + 徽章"等待确认"渲染正常, 无 console 错误。

## 后续优化: 时间线 + 分组 + 状态高亮(2026-08-22, v1.4.0)
综合方案: 时间线(1) + 过渡高亮+分组(3), WebSocket(2) 后续单独做。
- 后端: store 新增 `events` 表(PRU 自增 PK), 记录 cmd/edit/conn/state 四类行为事件(去重: 值变化才写)
- 后端: `agentMonitor` 加 prevCmd/prevEditFile/prevConn/prevState 去重字段; `recordEvents()` 在 scan 写库
- 后端: 新增 `GET /api/events?pid=&limit=`, `PruneEvents` 随进程清理
- 前端 App.vue: 卡片按工具分组(组头状态计数 badge) + 展开抽屉含"信息/时间线"两标签
  + 状态变化 flash 高亮动画(轮询对比 prevState)
- 单元测试 store_test.go: RecordEvent/RecentEvents/PruneEvents

真机验证(Playwright): 按工具分组正常、时间线抽屉渲染事件(时间戳+类型色点)、
状态高亮动画接入、无 console 错误、无空白框架。

## 前端 Vue3 重建(2026-08-22)
方案 A: Vue3 + Vite SPA 内嵌, 与 min-fileweb 一致。
- 新建 `frontend/`(Vue3 + vite, 仅 vue 依赖, 不加 element-plus/codemirror 保持轻量)
- `vite.config.js`: outDir `../backend/internal/server/web/dist`, base `./`, VITE_APP_VERSION 注入
- `App.vue`: 卡片网格 + 状态徽章(带圆点/色区分) + 搜索筛选 + 状态 chips(带计数) +
  点击展开 per-agent 详情 + transition-group 动画 + 2s 轮询
- 删除旧 `web/index.html`; `server.go` 改 embed `web/dist`; `.gitignore` 忽略 `web/dist/`
- `build.yml` CI 加 npm install + build 前端步骤(前端先于 Go 构建, 产物被 embed)
- 零 CGO 单二进制特性不变

真机验证: Playwright 截图确认 #app 渲染、卡片/筛选/搜索/版本号正常, 无 console 错误, 无空白框架。

## thinking 增强(2026-08-22, 真实 copilot 验证)
原 thinking 判定依赖 sys_enter_connect 一次性事件 + 进程无活动, 对真实 copilot 失效:
1. copilot 复用/持久 LLM 连接 → connect 只触发一次, 之后 eBPF 抓不到
2. copilot 推理期间持续写内部 UUID 状态文件 → recentEdit 恒真 → 永远 running

增强(全部单测覆盖):
- `hasLLMConn(pid, llmIPs)`: 扫 /proc/<pid>/net/tcp 看是否有到 LLM 的 ESTABLISHED 套接字(覆盖复用连接)
- `isOutboundTLS` + `isKnownLLMAgent`: LLM 代理工具外连公网 443/80 即判连 LLM(GitHub 超大 IP 段兜底)
- `thinkWindow=30s`: thinking 窗口放宽, 不依赖 connect 瞬时事件
- `activeWrite` 收窄为"写用户文件"(纯读 IO 不算 active)
- `isTransientFile`: UUID 前缀/.tmp/.lock/.pyc 等内部文件不算编辑信号, 不阻断 thinking

真机验证: copilot 纯推理任务 2162713 显示
`state=thinking detail=调用 LLM 140.82.114.21:443`(GitHub Copilot 真实 IP)。

## 验证结果
- 单元测试: TestUpdateStateEditing/Thinking + 新增 TestUpdateStateThinkingReusedConn/
  TestUpdateStateThinkingOutboundFallback/TestHexToIPv4/TestIsOutboundTLS 全过
- 实机(模拟): last_file / last_conn 正确
- 实机(模拟): elkeid-agent editing
- Playwright: Web 渲染无错误
- **真机 copilot 1.0.80**: editing(写用户文件) + running(活跃/连LLM写文件) + thinking(连LLM仅写内部状态) 三种状态全部实时观测到

## 待办(发版前)
- [x] 提交(单 squash) + 打 tag v1.1.0 + 强推 + GitHub Release  ← 已完成
- [x] README 更新 eBPF 行为采集说明 + 隐私闸门  ← 已完成
- [x] 真机验证: 真实 copilot 的 editing/running 切换  ← 已完成
- [ ] (可选) thinking 实时验证: 需 copilot 任务中间有"纯推理无本地活动"的间隙(本机任务活动过密)

## 已知限制
- 本机有 elkeid-agent/irqbalance-ng 等系统进程会被 Match 命中(测试噪声), 真实部署无
- 沙箱后台进程无 stdin, read 立即 EOF, sleep 易被回收 → 无法稳定制造 idle 窗口验证 thinking/editing 实时切换(单测已覆盖状态机)
- copilot 任务中持续活动 → 多显示 running 而非 thinking(设计预期, thinking 需连 LLM 后无本地活动)
