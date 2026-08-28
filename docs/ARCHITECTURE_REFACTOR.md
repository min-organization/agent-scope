# agent-scope 架构重构方案: AgentSource 适配器抽象

> 背景: 已完成 hermes / copilot / claude 三者状态判定的逐 agent 调研与修复
> (claude 的 transcriptState 误判 waiting、copilot 的 events.jsonl 解析、hermes 的通用
> eBPF 推导)。当前状态判定逻辑分散在 `collector.go`(5 处 `if tool=="claude"` 硬分支)、
> `monitor.go`(copilot 特例 192 行)、`transcript.go`(claude/codex 专用)、`copilot.go`
> (copilot 专用), 新增 agent 需 5+ 文件联动且无编译期约束。本方案引入适配器接口,
> 消除硬分支, 提升扩展性、版本兼容与可测试性。

---

## 1. 现状评估(实测)

### 做得好的
- 分层清晰: `collector / config / store / ebpf / server / wss / notify` 职责内聚, 无循环依赖
- eBPF 零侵入采集, 不注入不控制(铁律落地)
- 前端状态枚举单一真相: `useAgentMon.js` 的 `states` + `i18n.js` 文案, 前端无 `tool` 硬编码(搜索 0 命中)
- 存储层已有 schema 迁移(`ensureAgentsSchema` 用 `ALTER TABLE ADD COLUMN`), 版本兼容思路存在

### 结构性债务
- **无 agent 适配器抽象**: 状态推导散落硬分支, 违反开闭原则(OCP)
- **claude 是一等公民硬编码**: `Collector.claudeTx` 是专用字段(34 行), 推导逻辑深埋主循环
- **后端 State 是裸 `string`**: `store.Agent.State string`, 注释残留已删的 `blocked/unknown/done`(20 行), 前后端靠约定维系
- **transcript 解析无版本路由**: claude 格式随版本变(`atis-latch`/`file-history-snapshot`), 靠宽松解析+兜底, 大版本变更易静默误判
- **codex 声明未实现**: `isKnownLLMAgent` 列了 8 个 agent, 但仅 claude/copilot/hermes 落地

---

## 2. 设计目标

1. **OCP**: 新增 agent = 加一个 Source 文件 + 注册一行, 不动 `collector.go` 主循环
2. **范式统一**: claude/copilot/hermes/codex 的状态推导收敛到同一接口, 各自内聚
3. **编译期约束**: agent 清单由注册表驱动, 漏注册编译报错(而非运行时静默)
4. **渐进迁移**: 每次只搬一个 agent, 不破坏现有运行; 保留通用兜底
5. **版本兼容**: transcript 解析按版本路由; 状态枚举类型化, 前后端契约可校验

---

## 3. P0 核心: AgentSource 接口

### 3.1 接口定义(新建 `collector/source.go`)

```go
// AgentSource 定义某类 agent 的状态推导范式。
// 每个 agent(claude/copilot/hermes/codex...) 实现自己的 Source, 注册到 registry。
// 主循环只通过接口交互, 不在 collector.go 写 if tool== 硬分支。
type AgentSource interface {
    // Tool 返回该 Source 负责的 agent 名(与 matchTool 命中名一致)。
    Tool() string

    // DeriveState 综合 proc 监控(m) + 转录本(tx, 可能 nil) + 超时阈值, 推导状态。
    // 返回: state(运行/推理/编辑/等待/空闲/错误)、detail(人类可读补充)、needsInput(阻塞等用户输入)。
    // 设计要点:
    //   - m 提供 eBPF 行为、pty 阻塞、LLM 连接等内核态信号(通用)
    //   - tx 提供该 agent 自身的转录本结构化信号(claude transcript / copilot events / ...)
    //   - 各 Source 自行决定如何组合两者(claude 用 transcript 覆盖 proc; copilot 完全信 events)
    DeriveState(m *agentMonitor, tx *transcriptInfo, idleNs int64) (state, detail string, needsInput bool)

    // ParseTask 提取给人看的任务摘要(可选, 默认返回 "")。
    ParseTask() string
}
```

### 3.2 注册表(消除硬分支)

```go
// registry 在 init() 或 New() 时填充, 主循环通过 tool 名查表。
var registry = map[string]AgentSource{}

func Register(s AgentSource) { registry[s.Tool()] = s }

func SourceOf(tool string) (AgentSource, bool) {
    s, ok := registry[strings.ToLower(tool)]
    return s, ok
}
```

### 3.3 主循环改造(collector.go)

**改造前**(当前, 硬分支):
```go
if tool == "claude" && c.claudeTx != nil && depth == 0 {
    txSt, txDetail := transcriptState(*c.claudeTx, 60e9)
    if txSt == "thinking" || txSt == "error" { st, cf, detail = txSt, "high", txDetail }
    else if txSt == "waiting" && st != "running" && st != "thinking" { st, cf, detail = "waiting", "high", txDetail }
}
```

**改造后**(经由接口):
```go
if src, ok := SourceOf(tool); ok && depth == 0 {
    sSt, sDetail, sNeed := src.DeriveState(m, c.txOf(tool), 60e9)
    if sSt != "" {
        st, cf, detail = sSt, "high", sDetail
        needsInput = sNeed
    }
}
```

> 注: `c.claudeTx` 专用字段迁移为 `c.txs map[string]*transcriptInfo`(按 tool 存各 agent 转录本),
> 或保留 claude 快捷字段但主循环不再直接调用 `transcriptState`。

### 3.4 各 agent Source 边界

| Agent | Source 实现 | 状态源 | 关键逻辑 |
|---|---|---|---|
| **claude** | `claudeSource` | proc + transcript | `transcriptState(tx)` 覆盖 proc; 活跃 tool_use→thinking, 超时 tool_use→waiting, 末行 assistant 非 tool_use→idle |
| **copilot** | `copilotSource` | events.jsonl | `parseCopilotState()` 直接返回 (state,task,file,needsInput), 完全信 events |
| **hermes** | `hermesSource` | proc(通用) | 复用现有 `monitor.updateState` 通用分支(running/thinking/editing/waiting/idle) |
| **codex** | `codexSource`(新增) | proc + transcript | 复用 claude 的 transcript 解析(claude/codex transcript 格式同源), 差异化点在版本/字段 |

`copilotSource.DeriveState` 实现极简:
```go
func (copilotSource) DeriveState(m *agentMonitor, tx *transcriptInfo, idleNs int64) (string, string, bool) {
    st, task, file, need := parseCopilotState()
    if st == "" { return "", "", false }
    return st, task, need
}
```
(现有 `parseCopilotState` 完全保留, 只是从 monitor.go 的特例搬进 copilotSource)

---

## 4. 迁移步骤(渐进, 不破坏运行)

1. **新建 `source.go`**: 定义 `AgentSource` 接口 + `registry` + `Register/SourceOf`
2. **新建 `claude_source.go`**: 把 `collector.go:240` 的 claude 分支 + `transcriptState` 调用收敛进 `claudeSource.DeriveState`
3. **新建 `copilot_source.go`**: 把 `monitor.go:192` 的 copilot 特例搬入 `copilotSource.DeriveState`(内部仍调 `parseCopilotState`)
4. **新建 `hermes_source.go`**: 包装现有通用 `updateState`(hermes 无特例, 薄封装)
5. **`collector.go` 主循环**: 用 `SourceOf(tool)` 替换 5 处 `if tool=="claude"`; `c.claudeTx` 改为 `c.txs[tool]`
6. **`init()`/`New()` 注册**: `Register(claudeSource{})` / `copilotSource{}` / `hermesSource{}` / `codexSource{}`
7. **保留通用兜底**: 未注册 tool 走现有 `updateState`(兼容未来未实现的 agent, 如 aider/gemini 先通用后精细)

每步独立可编译、可测试, 单 agent 灰度。

---

## 5. P1: 状态枚举类型化(前后端契约)

### 后端(`store.Agent.State`)
```go
type AgentState string
const (
    StateRunning  AgentState = "running"
    StateThinking AgentState = "thinking"
    StateEditing  AgentState = "editing"
    StateWaiting  AgentState = "waiting"
    StateIdle     AgentState = "idle"
    StateError    AgentState = "error"
)
// store.Agent.State 改为 AgentState; 删除注释里的 blocked/unknown/done(已删功能)
```
可选: `func (s AgentState) Valid() bool` 校验; 或用 `stringer` 生成。

### 前端契约校验
- `useAgentMon.js` 的 `states` 数组改为从同一份枚举生成(或 CI 加脚本校验前后端枚举一致)
- 避免"后端加状态前端不显示"的静默回归(你这几轮反复踩)

---

## 6. P2: transcript 解析版本化

`readTranscript` 增加版本路由:
```go
func readTranscript(path, tool string, monitors map[string]*agentMonitor) *transcriptInfo {
    ver := detectTranscriptVersion(path) // 从头部元数据 / 文件名 / 首行 schema 推断
    switch ver {
    case v1: return parseTranscriptV1(...)
    case v2: return parseTranscriptV2(...)
    default: return parseTranscriptV1(...) // 兜底最新已知版本
    }
}
```
避免 claude 大版本改 transcript schema 时"宽松兜底"静默误判。

---

## 7. P3: 配置 schema 版本 + 迁移

`config.go` 加 `Version int`; 加载时若旧版本则迁移(复刻 store 层的 ALTER 思路):
```go
func (c *Config) Migrate() error {
    if c.Version < 2 { /* 旧字段映射 */ c.Version = 2 }
    return nil
}
```

---

## 8. 回归测试策略

- **单元测试**: 每个 Source 独立测(同现有 `transcript_state_test.go` / `collector_test.go` 范式)
  - `claudeSource`: 活跃 tool_use→thinking / 超时 tool_use→waiting / 对话空闲→idle / API 错误→error
  - `copilotSource`: ask_user→waiting / permission→waiting / 空闲→idle
  - `hermesSource`: 通用 running/thinking/editing/idle
- **集成测试**: `SourceOf(tool)` 注册表完整性(所有 `isKnownLLMAgent` 列表里的 agent 要么有 Source 要么走兜底, 不遗漏)
- **端到端**: Playwright 验证页面渲染(0 console), 各 agent 状态真实显示
- **迁移安全**: 每搬一个 agent 后, `go test ./internal/collector/` 全绿 + dev-build 重启 + 真实 agent 状态核对

---

## 9. 风险与回滚

- **风险**: 主循环接口化初期可能漏搬某 agent 的特例 → 该 agent 状态退化
  - 缓解: 迁移期间保留通用 `updateState` 兜底; 每步单 agent 灰度 + 测试
- **回滚**: 每次迁移是独立 commit(按你"单 squash 前不提交"纪律, 工作区累积, 统一 squash), 出问题 `git stash` 或 revert 单个文件
- **零侵入铁律**: 重构只动状态推导的代码组织, 不改 eBPF 采集语义、不改"只读不控制"

---

## 10. 分阶段交付

| 阶段 | 内容 | 风险 | 验证 |
|---|---|---|---|
| **P0-a** | source.go 接口 + registry + 主循环接口化(claude/copilot/hermes 三个 Source 落地) | 中 | go test 全绿 + 真实三 agent 状态核对 |
| **P0-b** | codexSource 落地(复用 claude transcript 解析) | 低 | codex 状态显示 |
| **P1** | 状态枚举类型化 + 删 store 过时注释 + 前端契约校验 | 低 | 编译 + 渲染 |
| **P2** | transcript 版本路由 | 低 | 多版本 transcript 解析测试 |
| **P3** | 配置版本迁移 | 低 | 旧配置加载兼容 |

---

## 11. 待定决策点(需你拍板)

1. **接口方法粒度**: 当前 `DeriveState(m, tx, idleNs)` 一把梭, 还是拆 `DeriveState` + `ParseTask` 两个方法?(文档用拆分版, 清晰)
2. **`c.claudeTx` 专用字段**: 改为 `c.txs map[string]*transcriptInfo`(支持多 agent 各持转录本), 还是保留 claude 快捷字段?(推荐 map 化, 为 codex 铺路)
3. **未注册 agent 的兜底**: 走通用 `updateState`(推荐, 兼容 aider/gemini 先通用后精细) vs 直接忽略?
4. **P1 前端契约校验**: 是否现在就加 CI 校验脚本, 还是仅后端类型化?(推荐后端先行, 前端契约校验后续)
5. **执行节奏**: 按你纪律(P0-a 先落地 → 你审 → 再 P0-b/P1...), 还是一次性 P0 全做?

---

## 结论

当前架构基础设施分层合理, 但**核心状态判定缺乏抽象, claude 硬编码为一等公民**, 不符合"易扩展"最佳实践;
版本兼容薄弱(靠宽松解析兜底, 无版本路由/契约校验); 扩展新 agent 成本高易错。

**最大 ROI 的一步 = P0 的 AgentSource 适配器接口**: 一次性消除所有 `if tool==` 硬分支,
让 claude/copilot/hermes 状态推导各自内聚, 新增 agent 变成"加文件+注册", 且不破坏现有运行。
后续 P1/P2/P3 渐进补强类型安全与版本兼容。
