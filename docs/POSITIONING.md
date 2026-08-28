# agent-scope 定位与差异化分析（POSITIONING）

> 基于 2026-08 对 CLI 编码代理监控场景的实查整理，用于明确 agent-scope 的差异化定位。

## 1. 场景

2025–2026 年起，开发者/运维在服务器上并行跑多个 CLI 编码代理（Claude Code、Codex、GitHub Copilot CLI、aider、open-code…）成为常态。
这些代理是交互式 TTY 程序，常驻后台，人不一定在跟前。

**核心痛点**：一眼看全“哪些代理在跑、哪些在等我确认、哪些干完了在发呆”——而不是逐个切终端看。

## 2. 现有方案与缺口

| 方案 | 覆盖 | 机制 | 缺口 |
|------|------|------|------|
| **CC-Monitor**（expAdd3） | 仅 Claude Code | Hook + Transcript JSONL | macOS 菜单栏，非服务器 Web；单工具 |
| **claude-monitor**（Maciek-roboblog） | 仅 Claude Code | 官方 `--statusline` 钩子 | 偏用量监控（额度），非状态面板；单工具 |
| **tmux 插件 / 手动 `tmux capture-pane`** | 任意（通用） | 读 tmux pane | **依赖 tmux 常驻**，引入中间服务；本项目明确不引入 |
| **各代理自带 `--verbose`/`--json`** | 单工具 | 单次运行输出 | 非常驻状态，无 Web 面板 |
| **代理编排框架（agentic web UI）** | 自管 agent | 框架内调度 | 不是“监控已在跑的 CLI”，是“自己管 agent” |

**市场缺口**：**服务器侧、不依赖 tmux、多工具统一的实时状态面板**——基本空白。

## 3. agent-scope 的差异化定位

**一句话**：服务器上跑多个 CLI 编码代理时，一个不依赖任何中间服务、零侵入的实时状态面板。

差异点（相对现有方案）：

1. **不依赖 tmux/screen/supervisor**：用进程级信号 + eBPF + 直读 pty 设备，零中间常驻服务
2. **多工具统一**：Claude/Codex/Copilot/aider 一个面板看全（Claude/Codex 叠 transcript 提精度，其他 pty 启发式兜底）
3. **服务器 Web 页面**：Go 单二进制 + Vue3，复用 min-fileweb/tlog-web 工程套路，自洽部署
4. **零侵入**：不改代理、不碰 API key、不注入 wrapper；只读进程/pty/transcript
5. **隐私安全**：只读状态 + 末行摘要，不存全量终端流（避免泄露密钥/代码）

## 4. 能力映射

| 维度 | agent-scope | CC-Monitor | tmux capture | 代理自带 verbose |
|------|-----------|------------|--------------|------------------|
| 多工具统一 | ✅ | ❌ 仅 Claude | ✅ | ❌ |
| 不依赖中间服务 | ✅(进程+eBPF+pty) | ✅ | ❌ 需 tmux | ✅ |
| 服务器 Web 面板 | ✅ | ❌ macOS 栏 | 需自建 | ❌ |
| running 精确 | ✅(eBPF execve) | 部分 | 启发式 | ✅ 单次 |
| waiting/idle 细分 | ✅(pty 文本) | ✅(Claude 事件) | ✅(看 pane) | 单次 |
| 隐私(不存流) | ✅ | ✅ | ❌ 存 pane | 视配置 |

## 5. 满足度评估

| 场景 | 满足度 | 说明 |
|------|--------|------|
| 盯多个 Claude Code 会话状态 | ⭐⭐⭐⭐⭐ | transcript JSONL + hooks 精确，Web 一屏看全 |
| 混跑 Claude + Codex + Copilot | ⭐⭐⭐⭐ | 进程级统一覆盖，Claude/Codex 提精度 |
| 不想装 tmux 就想监控 | ⭐⭐⭐⭐⭐ | 核心卖点，进程+eBPF+pty 直读 |
| 合规审计(谁动了什么) | ⭐ | 不做全量回放（隐私），仅状态；需回放看 tlog-web |
| 远程/手机看代理状态 | ⭐⭐⭐⭐ | Web 页面，但无移动端适配 |

**结论**：agent-scope 精准满足“自托管服务器上多 CLI 编码代理的轻量实时状态监控、不依赖中间服务”这一细分场景。
不与 CC-Monitor 对标（它只管 Claude + macOS），而是卡位“服务器侧、多工具、零中间服务的状态面板”。

## 6. 技术约束（设计边界）

- eBPF 需 root（CAP_BPF）——服务器运行可接受
- waiting/idle 细分依赖终端文本启发式，不同代理提示词需各适配一套正则（标置信度）
- 隐私优先：绝不全量落盘终端流；回放需求走 tlog-web 类专用方案
- MVP 先用进程级 + pty 文本，eBPF 与 transcript 适配作为精度增强叠加
