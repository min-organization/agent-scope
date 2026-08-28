# Contributing to agent-scope

Thanks for your interest in contributing! agent-scope is a zero-instrumentation, eBPF-powered observability tool for AI coding agents.

## Code of Conduct

All contributors must adhere to our [Code of Conduct](CODE_OF_CONDUCT.md). Be respectful, inclusive, and constructive.

## How to Contribute

### Reporting Bugs

- **Check existing issues** first to avoid duplicates.
- Include:
  - OS / kernel version (`uname -a`)
  - agent-scope version (`/healthz` endpoint or binary version)
  - Steps to reproduce
  - Relevant logs and/or `/api/agents` response
  - Whether eBPF loaded successfully (check startup logs)

### Suggesting Features

- Open a **Feature Request** issue describing the problem you're solving, not just the solution.
- Keep the **zero-instrumentation** principle in mind — agent-scope is a passive observer.

### Pull Requests

1. **Fork and branch** — `git checkout -b feat/your-feature` or `fix/your-bug`.
2. **Code style** — Follow Go `gofmt` for backend; Vue 3 `<script setup>` conventions for frontend.
3. **Build** — Ensure both frontend and backend compile:
   ```bash
   cd frontend && npm ci && npm run build && cd ..
   cd backend && go build ./...
   ```
4. **Test** — Run existing tests:
   ```bash
   cd backend && go test ./...
   ```
5. **Commit** — Write clear commit messages. Squash related commits before opening the PR.
6. **PR** — Link to the related issue. Keep PRs focused on a single fix/feature.

## Development Setup

```bash
# Prerequisites
# - Go 1.25+
# - clang (for compiling eBPF programs)
# - Node.js 20+ & npm

git clone https://github.com/min-organization/agent-scope.git
cd agent-scope

# Build frontend
cd frontend && npm ci && npm run build && cd ..

# Compile eBPF
cd backend/internal/ebpf/bpf && clang -O2 -target bpf -c agent_mon.bpf.c -o agent_mon.bpf && cd ../../../..

# Build backend
cd backend && CGO_ENABLED=0 go build -o agent-scope . && cd ..

# Run locally
sudo ./backend/agent-scope -config backend/agent-scope.yaml.example -db run/agent-scope.db -addr :8090
```

## Project Structure

```
backend/
├── main.go                           # Entry point
├── internal/
│   ├── collector/collector.go     # Core: scan, state inference, data collecton
│   ├── collector/collector_test.go   # Unit tests
│   ├── config/config.go              # Configuration loading
│   ├── ebpf/ebpf.go                  # eBPF monitor (cilium/ebpf)
│   ├── ebpf/bpf/                     # eBPF C programs
│   ├── notif/notify.go             # Alert notifier
│   ├── server/server.go              # HTTP + WebSocket server
│   ├── server/web/                   # Embedded Vue 3 frontend (built)
│   ├── store/store.go                # SQLite storage
│   └── wss/wss.go                     # WebSocket hub
frontend/
├── src/
│   ├── App.vue                        # Main applicatin
│   ├── composables/useAgentMon.js     # Shared state (singleton composable)
│   ├─ components/AgentNode.vue       # Recursive tree node component
```

## Key Principles

- **Zero instrumentation** — Never inject hooks into agents, never modify their behavior.
- **Privacy-first** — Only collect metadata (syscall types, basenames). Never capture file contents, PTY bytes, or network payloads.
- **eBPF affinity** — eBPF is the star. Code readability is secondary to kernel-level performance and safety.

## Questions?

Open a [Discussion](https://github.com/min-organization/agent-scope/discussions) or join the community.