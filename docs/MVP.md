# agent-scope MVP 设计

> 基于 docs/PROJECT.md 调研结论的最小可行版本。目标：本地可跑、不依赖 tmux 等中间服务、
> 用简单 Web 页面展示服务器上 CLI 编码代理的 running/waiting/idle 状态。
> 本文档为 MVP 实现契约，非最终代码。

## 0. 实现状态（2026-08-19）

MVP 已实现并**本地验证通过**：

- **eBPF 是 running 主信号**（非"不纳入"）：实测 pty 主设备多读者竞争（script/tmux/终端持有主侧消费数据），
  agent-scope 作为第二个读者读不到。改用 `tracepoint:syscalls/sys_enter_write` 按 pid 零竞争抓 pty 写
  （时间戳 + 末行 200B），`cilium/ebpf` 纯 Go 加载预编译 `.bpf`（clang 仅开发机构建用，CI 只需 go build）。
- **transcript JSONL（Claude/Codex）** 零竞争精确，优先于进程级（避免重复显示）。
- **pty 读取降级**为 eBPF 不可用时的 fallback（无 eBPF 且无 pty → blocked/low）。
- 状态机：running（活跃/有子进程）> waiting（末行含权限词）> idle（静止）> blocked（完全无法观测）。
- 单元测试 `collector_test.go` 覆盖 5 个状态分支，全过；eBPF running 抓取出实测可用。

## 1. MVP 范围

### 纳入
- 进程级状态判定（running / waiting / idle）—— 核心，零依赖
- **pty 持续监测（带时间戳 ring buffer）** —— running 主信号（思考态靠输出活跃度，见 §4 复审修正）
- **transcript JSONL 适配（Claude/Codex）** —— 免费精确的 running 信号源，MVP 纳入（Claude/Codex 优先用，其他工具用 pty）
- Web 卡片列表（工具 / 状态色块 / 末行摘要 / 最后活动 / 置信度）—— Vue3
- SQLite 存状态快照（采集 goroutine 写，页面读）
- 单台服务器

### 不纳入（后续增强，不在 MVP）
- **eBPF**（execve/pty write 跟踪）—— pty 持续 reader 已能拿输出时间戳，eBPF 是可选精度增强（免 reader 竞争）
- 全量终端回放 —— 隐私风险，不做
- 全量终端回放 —— 隐私风险，不做
- 多服务器 —— 单台

## 2. 技术栈（复用 min-fileweb / tlog-web 套路）

- 后端：Go + Gin，`//go:embed` 前端，纯 Go SQLite，零 CGO
- 前端：Vue3 + Element Plus（卡片列表，统一视觉）
- 采集：独立 goroutine 每 `interval` 秒扫 `/proc`
- 部署：单二进制 `./agent-scope -c agent-scope.yaml`；docker compose 可选（需 `privileged` 或 `CAP_SYS_PTRACE` 读其他进程 /proc，MVP 用 root 跑即可）

## 3. 代理识别

扫描 `/proc/[0-9]*/`：
- 读 `/proc/<pid>/cmdline`，匹配配置 `collect.match` 关键词（默认 `claude` / `codex` / `copilot` / `aider`）
- 匹配到的进程 = 一个 agent 会话，key = `pid`（进程退出即消失；重启换新 pid）
- 取工具名：cmdline 首个可执行 basename（如 `claude` / `codex`）

## 4. 状态机（MVP 判定逻辑）

```
对每 pid:
  # 持续监测得到的信号(见 §5 pty 持续 reader):
  last_output = pty 最近一次输出时间戳(持续 reader 或 eBPF 抓 pty write 得到)
  pty_active = (now - last_output) < idle_seconds

  if pty_active:
      # 有输出 = 在干活(思考流式输出 / 执行工具都在输出)  ← running 主信号
      state = running
  else:
      # pty 静止 (含"思考中但恰好没新 token"的短暂窗口, 由 idle_seconds 容忍)
      text = last_pty_lines(pid)          # 持续 reader 的 ring buffer 末行
      if 末行匹配 waiting_keywords:
          state = waiting                 # 停在确认提示符
      elif 末行是干净提示符(> / $ 且无等待词):
          state = idle                     # 干完在等你
      else:
          state = running                  # 静止但非提示符, 保守判 running(防漏思考态)

  # 辅助强信号(覆盖"执行工具"场景, 可选):
  if 有活跃子进程(bash/命令在跑, 见 §3 修正):
      state = running                     # 工具执行中, 强信号
```

### 判定要点（实现注意 / 复审修正）
- ⚠️ **致命点（复审实测发现）**：大模型 agent 在"思考 / 等 LLM API 响应"时，
  主进程是 **S 睡眠（网络 recv / hrtimer）**，**无子进程，CPU≈0，task 数=1**。
  PoC 验证：python `sleep` 模拟思考 → stat=S / wchan=hrtimer / tasks=1。
  若用"stat=='R' 或 child_active → running"判定，**思考态会被误判成 blocked（漏掉 running）**，
  而 agent 一半时间在思考。故 **running 必须以 pty 输出活跃度为主信号**，进程信号仅辅助。
- `tasks > 1` 不等于有子进程：`/proc/<pid>/task` 是线程，子进程是独立 pid（ppid==pid）。
  子进程检测应扫 `/proc` 找 `ppid==pid` 且 comm 非 agent 本身的进程，而非数 task 数。
- `wchan` 不稳定（read/futex/poll/ep_poll 因工具/内核而异），不单看。
- `stat=='R'` 持续 → running（代理在算/在跑工具）
- child_active 用扫描 `/proc/<pid>/task/*/stat` 或 `/proc/<pid>/task` 下子线程 + 检查是否 spawn 了 bash/外部命令（MVP 简化：task 数 > 1 即认为可能有活动，配合 CPU 时间增长更稳）

## 5. pty 监测（MVP 关键实现点，复审修正）

running 判定依赖"pty 输出活跃度（带时间戳）"，故 **MVP 必须持续监测 pty 输出**，而非采样式读（采样式拿不到输出时间维度，思考态会漏）。

### 5.1 持续 reader（每个 agent 一个 goroutine）
- 代理主进程 fd/0 符号链接指向 `/dev/pts/N`（master 侧回显设备）
- 对每个匹配 agent，起一个持续 reader goroutine：`tail -F` 或直接 `Read` `/dev/pts/N`，
  维护**带时间戳的 ring buffer**（末 `pty_ring` 行 + 每行 `last_output` 时间）
- `last_output = ring buffer 最新一行的时间` → 供 §4 状态机判 pty_active
- reader 仅存末行摘要（截断 200 字符），**不存全量流**（隐私）

### 5.2 竞争处理
- 多个 agent 各自 reader 独立（不同 /dev/pts/N），无跨 agent 竞争
- 与 agent 自身读 pty 的竞争：读 master 侧（`/dev/pts/N`）是回显，不消费 agent 输入，安全
- 若 `/dev/pts/N` 不可读（无 pty / 权限 / 管道），该 agent 退化为**仅靠进程信号**：
  pty_active 恒 false → waiting/idle 合并为 `blocked`（标低置信），running 仅靠子进程强信号

### 5.3 eBPF 替代（可选，更稳）
- eBPF 跟踪 `pty_write` 系统调用，内核态拿每次输出时间戳，免持续 reader
- 精度更高、零竞争，但需 root（CAP_BPF）。MVP 可用持续 reader 起步，eBPF 作增强

### 5.4 transcript JSONL 适配（Claude/Codex，强烈建议纳入 MVP）
- **Claude Code**：`~/.claude/projects/<id>/transcript.jsonl` 每行带时间戳 + 类型
  （`assistant` 流式 delta / `tool_use` / `tool_result` / `system`）
  - 有最近 `assistant`/`tool_use` 且 < idle_seconds → running（思考/执行都在写）
  - 收到 `session_stop` / `notification` 事件 → waiting/idle
  - **免费、精确、无 pty 竞争**，是 Claude running 判定的最佳信号源
- **Codex**：`~/.codex/sessions/*.jsonl` 类似（近似）
- MVP 建议：**Claude/Codex 优先用 transcript 判定，其他工具用 pty 持续 reader**；
  transcript 命中时状态置信度标 `high`，pty 命中标 `medium`

## 6. 项目结构

```
agent-scope/
├── backend/
│   ├── main.go                  # flag(-addr) + embed + gin 启动
│   ├── internal/
│   │   ├── config/config.go     # 单 YAML 权威配置
│   │   ├── collector/collector.go# /proc 扫描 + 状态机 + pty 采样
│   │   ├── store/store.go       # SQLite(agents 表: pid/tool/state/last_text/updated_at)
│   │   └── server/server.go     # Gin 路由 + /api/agents + Vue 静态
│   └── frontend/dist/           # Vue3 build(embed)
├── frontend/                    # Vue3 + Element Plus
│   └── src/...
├── agent-scope.yaml.example
├── docker-compose.yml           # 可选
└── docs/                        # PROJECT / POSITIONING / MVP
```

## 7. API

- `GET /api/agents` → `[{pid, tool, state, last_text, updated_at}]`
- `GET /healthz` → `{status:"ok"}`
- 页面 `/` → 卡片列表，前端每 2s 轮询 `/api/agents`（MVP 用轮询，简单；后续可改 SSE）

## 8. 配置（agent-scope.yaml，复用 min-fileweb 风格）

```yaml
server:
  addr: ":8090"
collect:
  interval: 2            # 采集间隔(秒)
  match:                 # 代理可执行名关键词
    - claude
    - codex
    - copilot
    - aider
  pty_ring: 200          # pty 采样末行数
  idle_seconds: 5        # 无输出静止多久判 blocked
waiting_keywords:        # waiting 启发式(可按工具细分, MVP 先用统一列表)
  - "Y/n"
  - "yes/no"
  - "Proceed?"
  - "Allow"
  - "[Y/n]"
  - "confirm"
  - "⏎"
```

## 9. 安全（必须）

- 只存 `last_text` 截断 200 字符，**绝不存全量 pty/终端流**（代理终端含 API key/代码/密钥）
- pty 持续 reader 仅维护内存 ring buffer（末行摘要），不落盘全文
- transcript JSONL 只读（不修改代理文件），且仅取状态/时间戳，不缓存全量对话内容
- MVP 页面无鉴权（内网假设）；如需外暴露，复用既有 JWT/session 模式

## 9.5. transcript 适配的隐私与权限
- 读 `~/.claude/projects/*/transcript.jsonl` 需 agent-scope 运行用户能访问该路径（通常与 agent 同用户运行，或 root）
- 仅解析状态，不复制/外发对话内容；页面不展示 transcript 全文

## 10. 本地测试计划

1. **无真实代理时**：用 `bash -i` 模拟，cmdline 注入关键词（如 `bash -c '...agent-sim...'` 或重命名匹配）验证三类状态
   - running：`bash` 里跑 `while true; do sleep 0.3; done`（task 活跃 + CPU）
   - waiting：`bash` 里 `read -p 'Proceed? [Y/n] ' x`（阻塞 read + 末行有 Y/n）
   - idle：`bash` 提示符静止无活动
2. 启 agent-scope，页面验证三状态色块正确 + 末行摘要显示 + 2s 刷新
3. 验证 SQLite `agents` 表写入
4. （可选）本机装 claude/codex 真实跑一轮，验证实际匹配与状态

## 11. 实现风险（含复审修正）

- **⚠️ 思考态漏判（复审实测发现，已修）**：agent 等 LLM API 时主进程 S 睡眠、无子进程、CPU≈0，
  `stat=='R' 或 child_active` 判定会把它误判成 blocked。修正：running 改以 **pty 输出活跃度（时间戳）为主信号**，
  进程信号仅辅助。验证 PoC：python `sleep` 模拟思考 → stat=S / wchan=hrtimer / tasks=1。
- **持续 reader goroutine 泄漏**：每个 agent 起一个 reader，agent 退出时必须回收（否则 goroutine 泄漏）。
  用 pid 消失事件触发 reader 关闭 + context cancel。
- **pty reader 竞争**：读 master 侧 `/dev/pts/N` 是回显，不消费 agent 输入，安全；多 agent 各读各的 pts，无跨竞争。
  若某 pts 不可读 → 退化为仅靠进程信号（running 仅靠子进程强信号，waiting/idle 合并 blocked 标低置信）。
- **transcript 轮询开销**：Claude/Codex transcript JSONL 每次采集需 stat mtime + 读尾部（增量读，记 offset），
  避免全量重读。多会话时开销可控。
- **wchan 不稳定**：不同内核/工具 wchan 名不同 → 不单看，组合 pty 活跃度 + 子进程信号。
- **权限**：读其他进程 /proc 需同用户或 root；读 /dev/pts/N 需同用户或 root；读 transcript 需能访问 `~/.claude`。MVP 用 root 跑（服务器场景可接受）。
- **pid 复用**：进程退出重启后 pid 变，key 用 pid 即可（重启即新会话，符合直觉）。
- **waiting 关键词误判**：不同 agent 提示词不同（如 Codex 用 `Allow`、Claude 用 `Y/n`）。
  MVP 用统一列表 + 按工具叠加专属词；末行非提示符时保守判 running（防漏思考态）。

## 12. 完成标准（MVP 可交付）

- [ ] 本地起 agent-scope，能扫到匹配代理进程
- [ ] running / waiting / idle 三态在本机模拟下正确判定
- [ ] Web 页面卡片列表实时（2s）展示，色块 + 末行摘要
- [ ] SQLite 状态持久（重启 agent-scope 不丢最近快照，进程消失即清）
- [ ] 不依赖 tmux/screen/supervisor 等中间服务
- [ ] 不存全量终端流
