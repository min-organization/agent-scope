# agent-scope

> **Linux-only** · Zero-instrumentation observability for AI coding agents (claude / codex / copilot / aider, and more)

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![Platform](https://img.shields.io/badge/Platform-Linux%20(amd64%2farm64)-333?logo=linux)](https://github.com/min-organization/agent-scope)

**agent-scope** monitors AI coding agents running on your Linux server — in real time, with zero instrumentation and zero control over the agent. It captures system-call metadata via eBPF (write / execve / openat / connect), then aggregates them into human-readable states: `running` / `thinking` / `editing` / `waiting` / `idle`.

**Single static binary, zero dependencies.** Drop it on any Linux box (kernel ≥5.8 with BTF) and start monitoring in seconds.

> [!IMPORTANT]
> agent-scope is a **passive observer only**. It never modifies agent behavior, never opens PTY devices, never reads file contents, and never captures network payloads.

---

## Requirements

| Requirement | Minimum | Recommended |
|---|---|---|
| **OS** | Linux kernel ≥ 5.8 (eBPF CO-RE) | Linux ≥ 6.0 (better BTF) |
| **Architecture** | amd64 (x86_64) or arm64 (aarch64) | Same |
| **eBPF privilege** | `CAP_BPF` + `CAP_SYS_ADMIN` | root or `--privileged` container |
| **Disk** | 50 MB (binary + DB) | 200 MB (with history) |
| **Memory** | 20 MB idle | 100 MB (50+ agents) |
| **Go (build only)** | Go 1.25+ | Same |
| **clang (build only)** | clang 14+ (for .bpf.c) | clang 16+ |

> On kernels **without** eBPF/BTF (e.g. < 5.8, `CONFIG_DEBUG_INFO_BTF=n`), agent-scope auto-degrades to /proc-only mode with reduced precision.

> ARM64 (Raspberry Pi, AWS Graviton, Oracle Ampere) is **fully supported** — eBPF tracepoints are identical across architectures.

---

## Screenshots

<!-- TODO: Add real screenshot. Run agent-scope, open http://localhost:8090, save a screenshot here -->
| Process Tree | Kanban Board | Alerts |
|---|---|---|
|<!--( Screenshots placeholder — capture your own from http://localhost:8090 )-->|

---

## Features

- **🧬 eBPF-native** — Captures write / execve / openat / connect syscalls via cilium/ebpf tracepoints. No CGO, no kernel headers at build time.
- **🌳 Process tree** — Full parent-child process tree for every agent. Sub-agents, shell children, copilot MainThread — all mapped correctly.
- **🎯 6-state inference** — `running` / `thinking` / `editing` / `waiting` / `idle` / `blocked`, each with a confidence indicator.
- **🤖 Multi-tool** — Works with claude, codex, copilot, aider, hermes, opencode, gemini, and any custom tool via config.
- **🔌 Copilot native** — Reads `events.jsonl` as the authoritative state source, solving the "waiting for LLM" vs "waiting for user" ambiguity.
- **📡 WebSocket push** — Real-time state updates to the Vue 3 frontend, with 2-second polling fallback.
- **🔔 Anomaly detection** — Detects stuck agents, zombie processes, resource leaks, and duplicate sessions.
- **📋 Kanban UI** — Switch between process tree and state-column board view in one click.
- **🔍 Full-text search** — Filter agents by tool name, PID, file, connection, or task.
- **🔒 Privacy-first** — Metadata only. No file content, no PTY bytes, no network payloads.
- **⚡ Single binary** — `agent-scope` + SQLite. No Node.js, Python, or Docker required on the server.

---

## Architecture

```text
         ┌──────────────────────────────────┐
         │          eBPF (kernel)           │
         │  tracepoints: write execve       │
         │  openat connect                  │
         │  └─> beh_map (per-PID events)    │
         └────────────┬─────────────────────┘
                      │ poll (every 2s)
         ┌────────────▼─────────────────────┐
         │          Collector (Go)           │
         │  ┌──────┐ ┌──────┐ ┌──────────┐  │
         │  │/proc │ │JSONL │ │ Copilot  │  │
         │  │ tree │ │read  │ │events.jsl│  │
         │  └──┬───┘ └──┬───┘ └────┬─────┘  │
         │     └────────┼──────────┘          │
         │              ▼                     │
         │      updateState() → infer         │
         └────────────┬─────────────────────┘
                      │ SQLite + WebSocket
         ┌────────────▼─────────────────────┐
         │        Frontend (Vue 3)           │
         │  Tree · Kanban · Search · Alerts  │
         └──────────────────────────────────┘
```

---

## Quick Start

### 0. Prerequisite checks

```bash
# Check kernel version (≥ 5.8 required for eBPF)
uname -r

# Check for eBPF BTF support
cat /sys/kernel/btf/vmlinux > /dev/null 2>&1 && echo "BTF ✅" || echo "BTF ❌ (fallback to /proc-only)"

# Check the arch
uname -m   # should return x86_64 or aarch64

# If building from source, verify Go
go version    # should be Go 1.25+
```

### Option 1: Pre-built binary (recommended for new machines)

```bash
# Download the correct architecture
curl -LO https://github.com/min-organization/agent-scope/releases/latest/download/agent-scope-linux-$(uname -m).tar.gz
tar -xzf agent-scope-linux-$(uname -m).tar.gz
cd agent-scope-linux-$(uname -m)

# Run (config is auto-created from .example)
sudo ./agent-scope -config agent-scope.yaml.example -db agent-scope.db -addr :8090
```

### Option 2: Build from source

```bash
# Build prerequisites
sudo apt-get install -y clang      # Debian/Ubuntu; only if .bpf.c needs recompile
cd frontend && npm ci && npm run build && cd ..
cd backend && CGO_ENABLED=0 go build -o agent-scope . && cd ..

# Run
sudo ./backend/agent-scope -config backend/agent-scope.yaml.example -db run/agent-scope.db -addr :8090
```

### Option 3: Project script (auto-configures)

```bash
bash deploy/agent-scope.sh start      # start
bash deploy/agent-scope.sh status     # check status
bash deploy/agent-scope.sh restart    # restart
bash deploy/agent-scope.sh stop       # stop
```

### Quick validation

```bash
# After starting, verify healthz
curl http://localhost:8090/healthz
# Expected: {"status":"ok"}

# Check eBPF status in logs
grep -i 'eBPF' run/agent-scope.err.log 2>/dev/null || echo "eBPF OK (no errors)"

# Open the web UI in your browser
echo "Open http://$(hostname -I | awk '{print $1}'):8090 in your browser"
```

---

## State Reference

| State | Meaning | Confidence |
|---|---|---|
| `running` | Active syscall activity / live sub-processes | high |
| `thinking` | Connected to LLM API, no local file/cmd activity | high |
| `editing` | Recent source file writes detected | high |
| `waiting` | Halted at permission prompt (`Y/n` / `Proceed?` / `Allow`) | high |
| `idle` | No activity, awaiting user input | medium |
| `blocked` | Cannot observe (no PTY, no eBPF, no text) | low |

---

## Privacy & Security

agent-scope is designed as a **passive observer**. It only reports metadata:

| Domain | Captured | NOT Captured |
|---|---|---|
| Commands | Process name / executable basename | Full argv with secrets |
| Files | Basename only | Full path, file contents |
| Network | `domain:port` / resolved `IP:port` | Payload bytes |
| PTY text | Last 200 characters (best-effort) | Full output, credentials |
| eBPF | Syscall type + timestamp | Write buffers, arguments |

`behavior.capture` modes: `off` (no behavior capture) · `metadata` (default, metadata only) · `full` (reserved, will warn before enabling).

---

## Configuration

Full reference in [`backend/agent-scope.yaml.example`](backend/agent-scope.yaml.example):

```yaml
server:
  addr: ":8090"
collect:
  interval: 2             # seconds between scans
  match: [claude, codex, copilot, aider, ...]
  idle_seconds: 5
behavior:
  capture: metadata       # off / metadata / full
  edit_ext: [.go, .py, .ts, .js, ...]
  llm_hosts: [openai.com, anthropic.com, ...]
```

---

## API

| Endpoint | Description |
|---|---|
| `GET /` | Web UI (Vue 3 SPA) |
| `GET /api/agents` | Agent list with state, tree, events |
| `GET /api/alerts` | Recent anomaly alerts |
| `GET /api/agents/:pid/timeline` | Agent event timeline |
| `GET /healthz` | Health check `{"status": "ok"}` |

---

## FAQ

**Q: Does this work on macOS or Windows?**
A: No. agent-scope uses eBPF (Linux kernel ≥5.8) and /proc. For macOS, see [ccboard](https://github.com/FlorianBruniaux/ccboard) or [hoangsonww/Agent-Monitor](https://github.com/hoangsonww/Claude-Code-Agent-Monitor).

**Q: Does this require root?**
A: eBPF requires `CAP_BPF` / `CAP_SYS_ADMIN` (or root). If eBPF loading fails, agent-scope auto-degrades to /proc-only mode with reduced precision.

**Q: Can I monitor agents as a different user?**
A: Yes, as long as agent-scope can read their /proc entries. Root or `CAP_SYS_PTRACE` is recommended.

**Q: Does it work with Docker containers?**
A: Yes, with `--cap-add CAP_BPF --cap-add CAP_SYS_ADMIN` and host /proc mounted.

**Q: Will this slow down my agents?**
A: Negligible. eBPF tracepoints are sub-microsecond; /proc polling every 2s uses trivial CPU.

**Q: How does it know the agent is "thinking"?**
A: When eBPF detects a `connect` to a known LLM host and no local file writes follow. For copilot, events.jsonl is authoritative.

---

## Comparison with Similar Tools

| Feature | agent-scope | ccboard | hoangsonww/Monitor | agentsight |
|---|---|---|---|---|
| Zero instrumentation | ✅ eBPF + /proc | ✅ Transcript only | ❌ Hooks injection | ✅ eBPF only |
| Multi-tool | ✅ claude/codex/copilot/aider/... | ❌ Claude only | ❌ Claude only | ✅ CLI agents |
| User-wait detection | ✅ (permission prompt) | ❌ | ❌ | ❌ |
| Process tree | ✅ Full tree | ❌ | ❌ Flat | ❌ Partial |
| Cross-platform | ❌ Linux-only | ✅ macOS + Linux | ✅ macOS + Win + Linux | ❌ Linux-only |
| Binary size | ~12 MB | ~8 MB (Rust) | ~50 MB (Node) | ~10 MB |

---

## Known Limitations

- **False positives** — Non-agent processes whose names match `match` keywords will appear as agents in mixed-workload servers.
- **Copilot only** — Only copilot has native events.jsonl for state; other tools rely entirely on eBPF inference.
- **thinking detection** — If an agent writes to internal state files while waiting for the LLM, it may briefly show as `running` (mitigated by `isTransientFile`).

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md)

## License

[MIT](LICENSE) © 2026 min-organization
