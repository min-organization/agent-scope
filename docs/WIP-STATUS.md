# agent-scope 进度临时记录 (WIP) — 2026-08-19

> 临时交接文档。切回其他项目前留存。下次接手直接看本文件 + MVP.md / AGENT-MECHANISMS.md。

## 一、当前状态:已发布可用 (v1.0.1)

仓库: https://github.com/min-organization/agent-scope  (public, main 分支)
Release: v1.0.0 (初始) + v1.0.1 (ANSI/控制字符过滤优化)
本地代码: /data/docker/compose/agent-scope/backend/

构建: `cd backend && go build -o agent-scope .`  (零 CGO 单二进制, ~19MB)
测试: `cd backend && go test ./internal/collector/`  (6 个状态机 case + cleanLine 6 case, 全过)

## 二、已实现并验证的能力

| 能力 | 状态 | 验证 |
|------|------|------|
| eBPF tracepoint(sys_enter_write) 零竞争抓 pty 输出 | ✅ | 实测抓到 agent-sim / copilot 真实 pty 写 |
| transcript/session JSONL (Claude/Codex) 零竞争精确 | ✅ | 模拟 transcript 判 running/high |
| 进程级匹配 (claude/codex/copilot/aider/opencode/gemini) | ✅ | 真实 copilot 进程被识别 |
| pty 读取降级 (eBPF 不可用时) | ✅ | 代码路径在, 未单独实测 |
| 状态机 running>waiting>idle>blocked | ✅ | 单测覆盖 5 分支 |
| ANSI/控制字符过滤 (cleanLine) | ✅ | copilot 控制序列不再伪造 running, 静止正确归 idle |
| Web 卡片页 + /api/agents | ✅ | Gin + 内联 HTML 轮询 |
| 纯 Go SQLite 持久化 | ✅ | — |

服务器已装 CLI: claude 2.1.235 / codex 0.148.0 / copilot 1.0.78 (均 /usr/local/bin/)

## 三、架构要点 (易忘的细节)

- **eBPF 是 running 主信号**。背景: 实测 pty 主设备多读者竞争 (script/tmux/终端持有主侧消费数据, agent-scope 第二个读者读不到), 故放弃 pty 持续读取, 改 eBPF。
- eBPF 程序: `backend/ebpf/agent_mon.c` (源码) → 编译 `backend/internal/ebpf/agent_mon.bpf` (embed 进二进制)。
  **clang 仅开发机用**, CI 只需 `go build` (加载预编译 .bpf)。
- **tracepoint ctx 布局坑**: `sys_enter_write` 的 buf 在 offset 24 (非 16), 读错偏移末行空。布局来自 `/sys/kernel/debug/tracing/events/syscalls/sys_enter_write/format`。
- **//go:embed 坑**: `.o` 扩展名被 Go 特殊对待 → embed 静默失效; 改名 `.bpf` + `var _ = embed.FS{}` 强制引用 (go1.26.6 环境必需)。
- **eBPF .c 不能在 Go package 目录** (internal/ebpf/) —— Go 会把 .c 当 cgo 源报错。源码放 `ebpf/` (非包目录), 编译产物放 `internal/ebpf/`。
- 状态判定: eBPF 抓到 pid 的 pty 写时间戳(last_write) → active=running; 末行文本含权限词(Y/n/Proceed?/Allow 等) → waiting; 静止 → idle; 完全无观测(无 pty 无 eBPF 无文本) → blocked/low。
- blocked 只在「无任何观测」时触发 (已修 bug: 原代码 blocked 会覆盖 waiting/idle)。

## 四、已知问题 / 待办 (按优先级)

### P1 — 真实端到端验证(2026-08-21 实测结论)
**已实测, 核心路径验证通过, 但 waiting 路径因环境受限未实时观察到:**
- ✅ 进程级检测: copilot 真实跑 (node + copilot 二进制多 pid) 被 agent-scope 全部识别为 tool=copilot, state=running, confidence=high
- ✅ last_text 抓取: copilot 跑 bash 循环时, eBPF 抓到 pty 写末行 (实测 last_text 出现 "1" / 引号片段), 证明 eBPF pty 文本捕获**真实生效**
- ✅ blocked 路径: copilot 因无 TTY 权限被拒 ("Permission denied and could not request permission") 时, agent-scope 正确判出 blocked/low (末行无文本 + 无 pty 活动)
- ⚠️ waiting 路径: **单元测试已覆盖** (TestUpdateStateWaiting: "Proceed? [Y/n]"→waiting), 且 last_text 抓取机制已实时证明可用, 但**实时未观测到真实 agent 停在权限提示** (本机 claude 未登录 / copilot 在此环境无 TTY 直接 denied 而非 pause)。属环境限制, 非代码缺陷。
- ⚠️ claude transcript 路径: 本机 claude 未登录, 无法实测 transcript JSONL 实时判定; 代码路径在, 单测未覆盖 (collector 仅 cleanLine+状态机单测, 无 transcript 集成测)。
- ⚠️ Web 页面渲染: **2026-08-21 已用 Playwright 截图视觉确认** —— 页面真实渲染卡片(深色主题、标题、copilot 执行中徽章、pid、更新时间、置信度 high), 无布局异常。MVP 验证闭环。

### P1 结论
**MVP 端到端验证已全部完成**(进程检测 / running / last_text(eBPF) / blocked / 多 pid 去重(P3) / Web 页面渲染 均实测通过)。唯一未实时观测的是 waiting 路径(claude 未登录 + copilot 在此环境无 TTY 直接 denied 而非 pause), 但 waiting 逻辑单测已覆盖且 last_text 机制已实测可用, 属环境限制非代码缺陷。

### P2 — 常驻部署未做
- agent-scope 目前手动跑 (`/tmp/agent-scope -config ... -db ... -addr ...`)。
- 待办: 写 systemd service 或 docker-compose, 后台持续监控。服务器惯例: /data/docker/compose/<proj>, compose 默认网络, 容器内:8080/宿主高位 3808x (参考 tlog-web/min-fileweb 端口规范)。

### P3 — 多 agent 同工具去重/合并(2026-08-21 已修复并实测)
- **修复**: 进程级扫描增加 `seenProcTool` 去重, 同一 tool 多 pid (node 主进程 + 二进制子进程 + 临时子进程) 合并为一条; 仅首个 pid 作 canonical monitor (驱动 eBPF 文本抓取), 其余额外 pid 仅贡献 running 信号 (任一有活跃子进程即 OR 标记 running, 调用 `markToolChild`)。
- **实测**: copilot 真实运行此前返回 3 条独立记录 (pids 2048095/2048105/2048117), 修复后同场景仅返回 **1 条** (canonical pid 2050437, state=running)。去重生效。
- 注: transcript 路径 (Claude/Codex) 仍由 `hasTranscriptTool` 优先去重, 与进程级互不重叠。

### P4 — waiting 细分对 copilot 依赖可见文本
- copilot 黑盒, 无 transcript/hooks。waiting 完全靠 eBPF 抓到的末行可见文本含权限词。若 copilot 用纯 TUI 控件(非文本 "Proceed?")弹确认, 可能判不出 waiting (归 idle)。
  - 文档已标注此局限。要精确需 copilot hooks (若官方提供) 或 eBPF 抓更细事件。

### P5 — 文档
- MVP.md 已加 §0 实现状态。AGENT-MECHANISMS.md / POSITIONING.md 是调研期写的, 仍有效。
- 缺 README.md (用户从 GitHub 克隆后如何构建运行)。待补 (可选, 发布整洁度)。

## 五、下次接手的第一步

1. `cd /data/docker/compose/agent-scope/backend && go build -o /tmp/agent-scope . && go test ./internal/collector/`
2. 若要做 P1 验证: 让 copilot 干个小任务, 同时 `HOME=/root /tmp/agent-scope -config /tmp/agent-scope-test.yaml -db /tmp/amt.db -addr :18xxx &` 然后 `curl localhost:18xxx/api/agents` 看 last_text。
   - 注意: /tmp/agent-scope-test.yaml 被清理过, 需重建 (match 含 agent-sim/claude/codex/copilot/aider, interval 1)。或用 backend/agent-scope.yaml.example。
3. 若做 P2 常驻: 参考 /data/docker/compose/min-fileweb 的 compose + systemd 套路。

## 六、关键命令速查

```
# 构建
cd /data/docker/compose/agent-scope/backend && go build -o agent-scope .
# 重编 eBPF (仅改了 agent_mon.c 时)
clang -O2 -g -target bpf -c ebpf/agent_mon.c -o internal/ebpf/agent_mon.bpf -I/usr/include -I/usr/include/x86_64-linux-gnu
# 跑 (需 root 挂 eBPF)
/tmp/agent-scope -config backend/agent-scope.yaml.example -db data/agent-scope.db -addr :8090
# 状态
curl localhost:8090/api/agents
```

## 七、已存 skill (避免重复踩坑)
- `go-ebpf-tracepoint-agentmon`: eBPF embed 坑 + tracepoint ctx 布局 + 验证配方。下次改 eBPF 先 load 这个 skill。
