package collector

import "testing"

func TestIsCopilotPrivateExec(t *testing.T) {
	cases := []struct {
		name    string
		comm    string
		cmdline string
		want    bool
	}{
		// MainThread 原生执行体(comm=MainThread) -> 折叠
		{"mainthread-comm", "MainThread", "/usr/local/lib/node_modules/@github/copilot/node_modules/@github/copilot-linux-x64/copilot --resume", true},
		// copilot 原生二进制路径(即使 comm 不是 MainThread) -> 折叠
		{"native-binary-path", "copilot", "/usr/local/lib/node_modules/@github/copilot/node_modules/@github/copilot-linux-x64/copilot --resume", true},
		{"native-binary-arm64", "copilot", "/x/copilot-linux-aarch64/copilot --resume", true},
		// copilot 主 node 进程(node 启动器) -> 不折叠(它是 root)
		{"node-launcher", "node", "node /usr/local/bin/copilot --resume", false},
		// 普通 bash 工具子进程 -> 不折叠(应保留并归并到主节点)
		{"bash-tool", "bash", "bash -c 'sleep 1'", false},
		// 完全无关的进程 -> 不折叠
		{"hermes", "node", "node /usr/local/bin/hermes", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isCopilotPrivateExec(procNode{comm: c.comm, cmdline: c.cmdline})
			if got != c.want {
				t.Errorf("isCopilotPrivateExec(%q,%q) = %v, want %v", c.comm, c.cmdline, got, c.want)
			}
		})
	}
}
