# agent-scope 各 Agent CLI 官方机制调研

> 调研目的：通过各 agent CLI 官方文档，厘清“从哪个阶段、用什么方式、能获取什么信息”，
> 为 agent-scope 的状态判定选型提供依据。
> 来源：Claude Code 官方 Hooks 文档（docs.anthropic.com/en/docs/claude-code/hooks，2026-08 抓取）、
> Codex CLI GitHub README、GitHub Copilot CLI README、aider.chat 文档；
> 及前文 CC-Monitor / claude-monitor 社区实现确认。
> 本文档事实性结论，供 MVP 实现选型。

## 1. 总览表

| 工具 | 状态源（阶段） | 获取方式 | 侵入性 | 精确度 | 对应状态 |
|------|----------------|----------|--------|--------|----------|
| **Claude Code** | Hooks 事件（官方） | settings.json 配 hook 脚本 | 轻侵入（改配置） | 高（确定性事件） | 三态全 |
| **Claude Code** | transcript.jsonl | 轮询 `~/.claude/projects/<id>/` | 零侵入（只读） | 高 | running 主 |
| **Claude Code** | `--statusline` | statusline hook | 轻侵入 | 中（用量） | 非状态 |
| **Codex** | session JSONL | 轮询 `~/.codex/sessions/*.jsonl` | 零侵入 | 中 | running 近似 |
| **Codex** | `--json` 输出 | 启动参数（非交互） | 启动参数 | 中（单次） | 流式 |
| **Copilot CLI** | 无外部接口（LSP `/lsp`） | 仅 pty 监控 | 零侵入 | 低（启发式） | 三态启发 |
| **aider** | `--watch-files`（文件变更） | 仅 pty 监控 | 零侵入 | 低（启发式） | 三态启发 |

## 2. 各工具详解

### 2.1 Claude Code（机制最完整，官方状态源充分）

#### 阶段 A：Hooks 事件（确定性状态转换）★
- **官方文档事件列表**（docs.anthropic.com/en/docs/claude-code/hooks）：
  - `SessionStart` / `SessionEnd` —— 会话起止
  - `UserPromptSubmit` —— 用户提交指令 → **running 开始**
  - `PreToolUse` / `PostToolUse` / `PostToolUseFailure` —— 工具调用前/后 → **running（执行中）**
  - `PermissionRequest` —— 请求授权 → **waiting（等用户确认）**
  - `Elicitation` / `ElicitationResult` —— 向用户提问澄清 → **waiting**
  - `SubagentStart` / `SubagentStop` —— 子代理起止 → running
  - `TaskCreated` / `TaskCompleted` —— 任务起止 → running
  - `Stop` —— 一轮结束 → **waiting / idle**
  - `TeammateIdle` —— **空闲** → idle 直接信号
  - `Notification` —— 通知（常伴等待）
- **获取方式**：在 Claude Code `settings.json` 配 hook（命令脚本或 HTTP），事件触发时 Claude 调用
- **能拿**：精确的三态转换事件（running/waiting/idle 都有确定性信号）
- **侵入性**：轻（需改 agent 配置装 hook）
- **对 agent-scope 的价值**：**最精确的状态源**，装一个 hook 写 SQLite 即可零轮询判定。但需用户同意改 agent 配置。

#### 阶段 B：transcript.jsonl（流式转录，零侵入）★
- **路径**：`~/.claude/projects/<project-id>/transcript.jsonl`（每会话一个，实时追加）
- **格式**：每行 JSON，含 `type`（assistant / tool_use / tool_result / system）、`timestamp`、消息体
- **获取方式**：轮询文件（增量读，记 offset + mtime）
- **能拿**：
  - 最近 `assistant` / `tool_use` 且 < idle_seconds → **running**（思考/执行都在写）
  - 收到 `session_stop` / `notification` 类事件 → waiting/idle
  - 每条带时间戳 → 解决“思考态漏判”（见 MVP.md §4 复审）
- **侵入性**：零（只读文件，不改 agent）
- **精确度**：高（免费、无 pty 竞争）
- **对 agent-scope 的价值**：**零侵入的最佳 running 信号源**，MVP 建议纳入（Claude 优先用 transcript 判定）。

#### 阶段 C：`--statusline`（官方 rate_limits）
- 官方 statusline 钩子输出 rate_limits / 用量；`claude-monitor` 用 `--write-state` 输出机器可读快照
- 拿的是**用量/额度**，非工作状态 → agent-scope 不依赖（可后续做用量展示）

### 2.2 Codex（OpenAI）

- **session JSONL**：`~/.codex/sessions/<id>.jsonl`（流式，社区确认；官方 README 未详述格式）
  - 获取：轮询（增量读）
  - 拿：对话流（assistant/tool 调用）→ running 近似
  - 侵入性：零；精确度：中（无明确 waiting 事件，waiting 仍需 pty 提示词）
- **`--json` 输出模式**：非交互启动参数，结构化输出（单次运行，非常驻状态）
- **approval 模式**：交互时等用户确认（default 模式会暂停等授权）→ waiting，需 pty 读末行 `Allow`/`Y/n` 判定
- **对 agent-scope**：Codex 用 session JSONL 提 running 精度，waiting 用 pty 启发式兜底

### 2.3 GitHub Copilot CLI

- 基于 LSP（`~/.copilot/lsp-config.json`），交互中用 `/lsp` 命令看 LSP 状态
- **无外部状态/事件接口** → 黑盒
- 获取状态只能靠 **pty 持续监测**（读末行提示词 `Y/n`/`Allow` + 输出活跃度）
- 侵入性：零（pty）；精确度：低（启发式，waiting/idle 合并 blocked 标低置信）

### 2.4 aider

- `--watch-files`：监控仓库文件变更，AI 注释触发自动修改（**非状态查询接口**）
- 无状态/事件接口 → 黑盒，只能 pty 监控
- 侵入性：零（pty）；精确度：低（启发式）

## 3. 信息获取方式分类（按侵入性）

| 方式 | 侵入性 | 说明 | 适用工具 |
|------|--------|------|----------|
| **进程级 `/proc`** | 零 | stat/wchan/task/子进程树 | 全部（辅助信号） |
| **pty 持续监测** | 零 | 读 `/dev/pts/N` 回显 + 时间戳 ring buffer | 全部（通用 baseline） |
| **读 transcript/session 文件** | 零 | 轮询 JSONL | Claude / Codex |
| **装 Hook（settings.json）** | 轻 | 事件触发写状态 | Claude（最精确） |
| **启动参数（--json/--statusline）** | 启动参数 | 需 agent 以此参数启动 | Claude / Codex |

## 4. 对 agent-scope MVP 的选型建议

### 零侵入 baseline（所有工具通用，MVP 必做）
- 进程级 `/proc` 信号（辅助：执行工具时子进程强信号）
- **pty 持续监测**（带时间戳 ring buffer）→ running 主信号（解决思考态漏判）
- pty 末行关键词 → waiting/idle 细分

### 精确增强（可选启用，零侵入优先）
- **Claude Code**：优先用 `transcript.jsonl` 判定 running（免费、精确、无竞争）
  - 若用户愿装 hook：用 Hooks 事件 → 三态确定性（最准，但改配置）
- **Codex**：用 `session JSONL` 提 running 精度
- **Copilot / aider**：无原生源，pty 启发式兜底（标低置信）

### 设计原则（贴合用户约束）
- **不依赖 tmux/screen 等中间服务** ✅（用 /proc + pty + 原生文件）
- **零侵入优先**：MVP 默认纯 /proc + pty；transcript/session 是只读文件，也算零侵入
- **轻侵入可选**：Hooks 作为“精确模式”开关（用户同意装时启用）
- **隐私**：只读状态/末行，绝不存全量终端流/对话全文

## 5. 关键事实确认（复审修正关联）

- ⚠️ **思考态漏判**：agent 等 LLM API 时主进程 S 睡眠、无子进程、CPU≈0 → 纯进程信号判不出 running。
  解决：running 以 **pty 输出活跃度 / transcript 时间戳** 为主信号（MVP.md §4 已修）。
- **Claude Hooks 是确定性三态源**：`UserPromptSubmit`/`PreToolUse`→running，`PermissionRequest`/`Elicitation`→waiting，`Stop`/`TeammateIdle`→idle。
  若装 hook，agent-scope 可零轮询精确判定（但需改 agent settings.json）。
- **Copilot/aider 黑盒**：只能 pty 启发式，waiting/idle 合并为 blocked 标低置信是可接受的降级。
