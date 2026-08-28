# agent-scope 优化实施计划

> 版本基线: v1.8.25(零侵入 eBPF 进程树 + /proc,实时状态 + copilot events.jsonl 权威 + 异常检测 + WebSocket 推送 + 多工具统一 + 前端树)
> 生成日期: 2026-08-23
> 原则: **守住零侵入 eBPF/进程级,不引入 Claude Code hooks / transcript.jsonl 依赖**;**只采集不控制**

---

## 一、调研结论(高 star 同类项目"方案好"点)

| 项目 | ★ | 语言 | 值得借鉴的亮点 | 是否契合零侵入 |
|---|---|---|---|---|
| hoangsonww/Claude-Code-Agent-Monitor | 935 | TS/React | Kanban 看板(按状态分列)、Web-Push 通知+声音、健康分环、插件市场、VS Code/桌面端 | Kanban/通知✅;插件市场/hooks❌ |
| FlorianBruniaux/ccboard | 91 | Rust 单二进制 | 成本/token 分析+预算告警、**审计日志(凭据/破坏性命令检测)**、FTS5 全文搜索、子 agent 树、多工具导入 | 审计日志✅;成本需 transcript❌ |
| 0x0funky/Agentinel | 91 | Py | 进程树+项目分组+空闲检测、**规则引擎(泄漏/僵尸/重复会话/内存压力)**、AI 对话分析、托盘 mini panel | 规则引擎✅;AI 分析可选 |
| JayantDevkar/claude-code-karma | 317 | Py | 会话时间线(prompt/tool/thinking 时序)、Tasks 流视图、Files 表、Ticket 关联 | 时间线回放✅;Ticket 关联❌ |
| onikan27/claude-code-monitor | 304 | TS | 手机 Web + 远程响应 permission 弹窗、Serverless | 移动查看✅;远程控制❌(违"只采集") |
| mixpeek/amux | 365 | Rust | 多 agent 编排 board、workers、groups、文件系统浏览 | 看板思路✅;编排/控制❌ |

**核心差异**:竞品多依赖 Claude Code hooks/transcript(只认 Claude Code)。agent-scope 优势是零侵入进程树,能监控任意进程级 agent。优化只取契合项。

---

## 二、agent-scope 现状能力盘点

### 已有能力
- 零侵入 eBPF 采集(`execve`/`openat`/`connect`/`write` 元数据)+ `/proc` 进程树
- 实时状态推导:`editing`/`thinking`(调 LLM)/`running`/`waiting`(等用户输入)/`idle`/`blocked`
- copilot 状态以 `events.jsonl` 为权威(方案 A)
- 异常检测:`detectAnomalies` 检测卡死/无响应/阻塞
- WebSocket 实时推送前端(`/api/agents` 树快照)
- 多工具统一(claude/codex/copilot/hermes/aider/openclaw)
- 前端:进程树(折叠/展开/搜索 X/空状态区分/时间线面板)

### 缺口
成本/token、历史会话库、Kanban 视图、移动端、审计日志(凭据/破坏性命令)、全文本搜索、健康分/图表、规则引擎(泄漏/僵尸/内存)、通知(Web-Push/声音/Webhook)、Ticket 关联

---

## 三、优化项(分级)

### P0 — 高价值、零侵入、直接可做

#### P0-1 Kanban 状态看板
- **借鉴**: 935★ Agent-Monitor 的 Kanban(按状态分列)
- **目标**: 前端新增看板视图,agent 按 `state` 归列(运行中/思考中/等待输入/空闲/异常),一眼看健康分布;"等待输入"列突出需处理项
- **实现要点**:
  - 前端 `App.vue` 顶栏新增视图切换(tab):`树形` / `看板`
  - 看板列定义(复用 `state` 枚举 + 异常标志):
    - `运行中`(running) / `思考中`(thinking) / `等待输入`(waiting) / `空闲`(idle) / `异常`(有 active alert)
  - 数据:复用 `/api/agents` 返回的 `state`/`needs_input`/`alerts`(已有 `RecentAlerts` 接口)
  - 卡片:显示 tool / pid / 最近文件 / 状态 chip;点击展开同树形时间线(复用现有 `open` 逻辑)
  - 复用 v1.8.20 的 `rootNodes` 维度(只看主 agent,子进程不在看板重复)
- **涉及文件**: `frontend/src/App.vue`(视图切换+看板渲染)、`frontend/src/components/AgentNode.vue`(卡片复用)、`frontend/src/composables/useAgentMon.js`(state 分组 computed)
- **验收**: 切到看板视图,5 类状态列正确归列;等待输入列有高亮;点击卡片展开时间线;无 console 错误(Playwright 真机验证)

#### P0-2 安全审计日志:凭据/破坏性命令检测
- **借鉴**: ccboard 审计日志(凭据检测、破坏性命令告警)
- **目标**: 检测 agent 命令行/路径中的凭据泄露与破坏性操作,写入审计告警(复用 `detectAnomalies` 通道)
- **实现要点**:
  - 后端 `collector.go` `detectAnomalies`(≈1316 行)扩展规则(仅 root agent,depth==0):
    - **凭据**: `execve` 命令行或 `openat` 路径含模式 `password=`/`token=`/`secret=`/`AKIA[0-9A-Z]{16}`/`.env`(写)→ `alert_kind=secret_leak`
    - **破坏性命令**: 命令行含 `rm -rf`/`git push --force`/`git reset --hard`/`DROP TABLE`/`mkfs`/`chmod -R 777` → `alert_kind=destructive_cmd`
  - 数据源:已有 `m.lastCmd`(execve basename,需扩为完整命令行?当前 `lastCmd` 仅 basename)→ **改为存完整 argv 摘要**(consumeEvent 里 `rawArg` 已是完整参数,需新增字段 `lastCmdLine` 存截断后的完整命令行,供审计匹配)
  - 写库:`c.store` 已有 alerts 表(`RecentAlerts` 接口),直接 `UpsertAlert`
  - 节流:复用 `lastAlert` 冷却 map(`(pid,kind)` 去重,避免每轮刷屏)
- **涉及文件**: `backend/internal/collector/collector.go`(`detectAnomalies` 扩展 + `agentMonitor` 加 `lastCmdLine` 字段 + `consumeEvent` 填充)、`backend/internal/store`(alerts 表已有,确认 kind 字段可存)
- **验收**: 制造 `bash -c 'echo password=xxx'` / `rm -rf /tmp/test` 常驻进程被 agent-scope 监控 → API `RecentAlerts` 出现 `secret_leak`/`destructive_cmd` 告警;无重复刷屏(冷却生效);前端看板"异常"列/告警区可见

#### P0-3 规则引擎:泄漏/僵尸/重复会话/内存压力
- **借鉴**: Agentinel 规则引擎(泄漏/僵尸/重复会话/孤儿/内存压力)
- **目标**: 用进程元数据扩展异常检测,从"卡死"单点扩到资源异常全貌
- **实现要点**(均在 `scan` 循环 root 处理 + `detectAnomalies`):
  - **僵尸**: root agent `now - lastOut > zombieSec`(默认 600s)且非 waiting/idle 真空闲 → `alert_kind=zombie`(长时间无输出却占资源)
  - **重复会话**: `matchedRoots` 中同 `tool` 出现 ≥2 个 root(非子进程)→ `alert_kind=duplicate_session`(提示可能重复启动)
  - **内存泄漏**: 读 `/proc/<pid>/status` 的 `VmRSS`,连续 N 轮(存 `m.rssSeries`)单调增长超阈值 → `alert_kind=mem_leak`
  - **内存压力**: root 自身 + 子树 RSS 总和超系统内存百分比(读 `/proc/meminfo`)→ `alert_kind=mem_pressure`
  - 数据:新增 `agentMonitor` 字段 `rssSeries []int64`(环形,存最近 M 次 RSS);`detectAnomalies` 读 `/proc/<pid>/status` 取 VmRSS(已有 `hasChild`/`lastOut` 等机制)
  - 节流:复用 `lastAlert` 冷却
- **涉及文件**: `backend/internal/collector/collector.go`(`detectAnomalies` 扩展 + `agentMonitor` 加 `rssSeries` + `scan` 里 duplicate_session 检测)、`backend/internal/store`(alerts kind 复用)
- **验收**: 起一个 `sleep` + 持续吃内存的进程(如 `python -c 'a=[];while: a.append("x"*10**6)'`)→ 出现 `mem_leak`/`mem_pressure` 告警;`sleep 9999` 长时无输出 → `zombie` 告警;同工具起两个 root → `duplicate_session` 告警

### P1 — 中价值,少量前后端

#### P1-1 健康分 + 图表(借鉴 935★ 健康环)
- 前端 ECharts:每 agent 健康分(综合 异常数/空闲时长/连接状态);全局状态分布 donut / tool 调用分布
- 文件: `frontend/src/` 新增 `components/HealthPanel.vue` + ECharts;数据用 `/api/agents` + alerts
- 验收: 健康分环 + 分布图渲染正确,无 console 错误

#### P1-2 历史会话库 + 时间线回放(借鉴 karma)
- 复用已有 `store`(agent 生命周期 + `recordEvents` 时间线);前端加"会话列表"页(按 UpdatedAt 倒序)+ 选中回放时间线
- 文件: `frontend/src/` 新增 `views/Sessions.vue`;后端确认 `ListTree`/`Prune` 保留窗口足够(当前 `Prune` 按 interval*2 清理,需延长或独立历史表)
- 验收: 历史 session 可列可回放;时间线事件按时间轴展示

#### P1-3 全文本搜索(借鉴 ccboard FTS5)
- 后端 agent 列表已可查;前端全局搜索扩到 进程名/cmdline/文件/状态(复用 v1.8.23 搜索框逻辑)
- 验收: 搜 `hermes`/`bash`/`kanban` 命中对应节点;空状态提示正确

#### P1-4 通知:Web-Push + 声音 + Webhook(借鉴 935★)
- 异常/等待输入时:前端 VAPID Web-Push + 提示音;可选 POST Webhook(企业微信/钉钉)
- 文件: 后端 `notify` 包已有 `Notifier`(确认接口);前端 Service Worker + 订阅;配置加 `Notify.WebhookURL`
- 验收: 触发告警 → 浏览器推送/声音;配 Webhook → 收到 POST

### P2 — 高价值但需取舍(可能突破零侵入)

#### P2-1 成本/token 估算
- 纯零侵入只能从 eBPF `connect`(LLM IP/时长)+ 写活动**粗估** LLM 调用次数/时长,不精确
- 精确 token 需**可选接入** transcript.jsonl(用户授权后,单工具 claude/codex)→ 建议作可选插件,非默认
- 决策点: 是否接受"粗估默认 + 精确可选"双模式

#### P2-2 移动端响应式 + 只读远程查看(借鉴 304★)
- 前端响应式;**仅做只读移动查看**,不做远程输入(守"只采集不控制")
- 决策点: 是否做移动适配

#### P2-3 Ticket 关联(借鉴 karma)
- 需外部集成(Linear/Jira/GitHub MCP),偏离"只采集本机 agent" → **不建议**默认

---

## 四、建议落地顺序

1. **P0-1 Kanban 看板**(纯前端,最快见效,差异化)
2. **P0-2 安全审计日志**(后端,复用 detectAnomalies,安全价值高)
3. **P0-3 规则引擎**(后端,资源异常全貌)
4. P1 按需求递进

每完成一项:构建 + `go test` + 重启 + Playwright/API 真实验证 + 单 squash 提交 + 干净 tag + push + GH release(遵循发布纪律)。

---

## 五、风险与约束

- **零侵入铁律**: 不读 pty 从属设备字节、不改 agent 行为;所有数据来自 eBPF 元数据 + /proc 只读
- **发布纪律**: 单提交(squash)+ 删测试 tag/Release;破坏性操作本地 review+实跑验证后再推
- **前端验证**: vite build 必须在 go build 之前;前端改动后必重编+重启+Playwright 验证渲染(拒绝"构建成功=完成")
- **P0-2/P0-3 需扩 `agentMonitor` 字段**: `lastCmdLine`(完整命令行摘要)、`rssSeries`(RSS 历史);注意 `consumeEvent` 当前 `lastCmd` 仅 basename,审计需完整命令行 → 新增字段不影响现有 `taskOf` 逻辑
