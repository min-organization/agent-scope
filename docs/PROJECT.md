# agent-scope 项目调研（立项草稿）

> 调研文档。目标：在**不依赖 tmux/screen/supervisor 等中间服务**的前提下，
> 监控服务器上运行的 CLI 编码代理（Claude Code / Codex / GitHub Copilot / aider 等）
> 的实时工作状态，并用简单 Web 页面展示。
>
> 状态分三类：**执行中（running）** / **等待确认（waiting）** / **空闲（idle）**。
>
> 本文档为调研结论 + 技术路线，非实现蓝图。实现细节以后续代码与 README 为准。

## 1. 背景与动机

服务器上并行跑多个 CLI 编码代理（claude、codex、copilot、aider、open-code…）是 2025–2026 年的新运维常态。
这些代理常驻交互式终端，人不在跟前时，需要知道：

- 哪个代理正在干活（running），哪个在等我确认（waiting），哪个干完了在发呆（idle）
- 能否一眼看到所有代理的状态，而不是逐个去终端看

现有做法的痛点：

- 代理本身是**交互式 TTY 程序**，不向外暴露结构化状态，`ps` 看不出它在跑还是在等
- 没有官方“status”查询接口（见 §3 官方现状）
- 现成开源监控面板要么只覆盖单一工具（Claude Code），要么依赖把代理塞进 tmux

## 2. 核心难点

监控“状态”的本质难点：**区分 waiting 与 idle**。

- **running**：代理在执行工具/命令 → 会 spawn 子进程（bash/命令）或 CPU 活跃
- **waiting**：代理刚问完“要不要继续？”→ 阻塞在 `read(stdin)` 等用户输入
- **idle**：代理完成上轮，在等你下指令 → 也阻塞在 `read(stdin)` 等用户输入

**waiting 与 idle 在操作系统层面是同一个状态：主进程阻塞在 read(stdin)。**
纯进程信号无法区分语义，必须看终端文本内容（waiting 时画面末尾有 `Y/n`/`Proceed?`/`Allow` 提示）。

## 3. 官方现状（调研结论）

| 工具 | 官方状态接口 | 原生状态源（不依赖中间服务） |
|------|--------------|------------------------------|
| **Claude Code** | 无 `status` 子命令 | ✅ **Hooks 系统**（PreToolUse/PostToolUse/Stop/Notification 确定性事件）+ **Transcript JSONL**（`~/.claude/projects/<id>/transcript.jsonl` 实时追加）|
| **Codex (OpenAI)** | 无 status 子命令 | ⚠️ Session JSONL（`~/.codex/sessions/*.jsonl` 流式），确定性不如 Claude hooks |
| **aider** | 无 | ❌ 无结构化，需读终端 |
| **GitHub Copilot CLI** | 无 | ❌ 黑盒，需读终端 |
| **open-code / 其他** | 无 | ❌ 需读终端或各自适配 |

**关键发现**：Claude Code 有官方可用的机器可读状态源（hooks + transcript），
不需 tmux。`claude-monitor`（PyPI, Maciek-roboblog）即利用官方 `--statusline` 钩子抓 `rate_limits` 并 `--write-state` 输出快照给外部工具。

## 4. 现成开源方案（调研结论）

- **CC-Monitor**（expAdd3，GitHub）：macOS 菜单栏，**Hook + Transcript 混合架构**实时追踪 Claude Code 多会话状态，完成时通知。机制 = Claude Code Hooks（确定性事件）+ Transcript JSONL（读 `~/.claude/...`）。**只覆盖 Claude Code**。
- **claude-monitor**（Maciek-roboblog）：用量监控伴侣，`--statusline` 抓官方 rate_limits，`--write-state` 机器可读快照。**只覆盖 Claude Code，偏用量。**
- **通用多工具统一面板**：调研未发现成熟开源项目覆盖 copilot/codex/claude 统一监控。这是个新场景。

**结论**：现成方案只解决 Claude Code 一种，且 CC-Monitor 是 macOS 菜单栏 Python 应用，不适合“服务器 Web 页面”场景。需自建，但可借鉴其 Hook+Transcript 思路。

## 5. 技术路线（不依赖中间服务）

约束：**不引入 tmux / screen / supervisor 等额外常驻服务**。

### 路线 A：进程级 + eBPF（推荐起步，零中间服务）

#### 5.1 进程级信号（`/proc`，无需任何依赖）
每个代理是一个进程树。读：

- `/proc/<pid>/stat` 第 3 列：进程状态 `R`(运行)/`S`(睡眠)/`D`(不可中断)
- `/proc/<pid>/wchan`：内核阻塞点（等输入时 = `read`）
- `/proc/<pid>/task/`：线程/子进程数
- `/proc/<pid>/fd/0`：是否连 pty（交互式判断）

判定：

- **running**：有活跃子进程树（代理执行工具时 spawn bash/命令），或主进程 CPU 活跃
- **blocked-on-input**：wchan = `read`，无活跃子进程 → 合并 waiting+idle

#### 5.2 eBPF（可选增强，已实测本机可用）
环境：Ubuntu 24.04，内核 6.8，`bpftrace` 已装，`/sys/kernel/btf/vmlinux` 存在（BTF 可用）。

PoC 实测：bpftrace 跟踪 `sys_enter_execve` 成功实时抓到 bash 循环每 0.3s spawn 的 `sleep`/`date` 子进程。
证明 eBPF 能精确观测“代理执行中时频繁 spawn 子进程”。

- 跟踪 `execve`：代理 running 时 spawn 工具 → execve 频繁 → running 置信度↑
- 跟踪 pty `write`：拿终端输出字节流 → 文本匹配 `Y/n`/`Proceed?`/`Allow` → waiting
  （等价于读 `/dev/pts/N`，均不依赖 tmux；eBPF 在内核态抓，更稳）

**eBPF 的位置**：作为 running 判定的增强（精确看到子进程 spawn）。MVP 可先用进程级，eBPF 可选提升。eBPF 需 root（CAP_BPF），服务器上可接受。

#### 5.3 waiting/idle 细分（必须读终端文本）
纯信号无法区分 waiting 与 idle。方案：**读 pty 文本流**做关键词启发式：

- waiting：`Y/n` / `yes/no` / `Do you want` / `Proceed?` / `Allow` / `[Y/n]` / `confirm` / `⏎`
- idle：画面静止 N 秒 + 末尾是干净提示符（`>` / `$`）
- 不依赖 tmux：直接 `cat /dev/pts/N`（内核设备）或 eBPF 抓 pty write，自维护 ring buffer（末 200 行）

### 路线 B：原生状态源适配（Claude Code 精确，其他启发式）
- **Claude Code**：读 `~/.claude/projects/*/transcript.jsonl` + 可选装 Hook 写状态到 SQLite
  - 最后一条是 `tool_use` 未 `tool_result` = 执行中
  - 收到 `Stop` / `Notification` 事件 = 等待确认/空闲
  - **精确，零中间服务（transcript 是工具原生输出，非中间服务）**
- **Codex**：读 `~/.codex/sessions/*.jsonl`（近似）
- **Copilot/aider/其他**：route A 的 pty 启发式兜底（标“低置信”）

### 路线选择
- **Claude Code 为主** → 路线 B（精确，官方推荐）
- **多工具统一** → 路线 A（进程级 + eBPF + pty 文本），Claude/Codex 额外叠加路线 B 提精度

## 6. 推荐架构（MVP）

```
agent-scope (Go 单二进制, Gin + Vue3 embed)
├─ collector (每 2s)
│   ├─ 枚举代理进程 (匹配 /proc 中 claude/codex/copilot/aider 可执行名)
│   ├─ 进程级: stat/wchan/task 数 → running / blocked-on-input
│   ├─ eBPF (可选): 跟踪 pid 树 execve 频率 → running 置信度
│   ├─ pty 文本: 读 /dev/pts/N, ring buffer 末 200 行 → 关键词匹配 → waiting/idle
│   └─ (Claude/Codex) 叠加 transcript JSONL 轮询 → 精确状态
├─ 状态机: running / waiting / idle (blocked 时标置信度)
└─ Web (Vue3): 卡片列表 (工具, 状态色块, 末行摘要, 最后活动)
```

复用既有项目套路（min-fileweb / tlog-web）：Go + Gin + Vue3、`//go:embed` 单二进制、纯 Go SQLite、零 CGO。

## 7. 安全（沿用项目偏好，必须）

- 监控页**只读状态 + 末行摘要**，**绝不存全量 pty/终端流**（代理终端含 API key / 代码 / 密钥）
- 若用 eBPF 抓 pty write，只做内存 ring buffer 匹配关键词，**不落盘全文**
- 状态页若需鉴权，复用既有 JWT/session 模式
- 回放终端全文（如需）必须加密 + 访问控制，MVP 不做

## 8. 待决问题

- [ ] 主监控对象：Claude Code 为主，还是 copilot/codex 也要通用？（决定精确度）
- [ ] 状态源：接受读 transcript 文件 + 可选装 hook（Claude 官方推荐）？还是只要纯进程级？
- [ ] eBPF 是否纳入 MVP（提 running 精度，需 root）
- [ ] 页面：状态卡片列表（MVP）就够，还是要点进看末 N 行？
- [ ] 本机装这些工具实测，还是另台？

## 9. 结论

**不依赖 tmux 等中间服务完全可行**：

- **running**：进程级（子进程树）+ eBPF（execve）双保险，精确
- **waiting/idle**：纯信号无法细分，需读 pty 文本（eBPF 抓 write 或 `/dev/pts/N` 直读，均不依赖 tmux）做关键词启发式
- 整体可用 Go 单二进制 + Vue3 实现，复用既有项目工程套路
