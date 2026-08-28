package collector

import "testing"

func TestIsHermesInfraExec(t *testing.T) {
	cases := []struct {
		name    string
		comm    string
		cmdline string
		want    bool
	}{
		{
			name:    "cua-driver 占位 node",
			comm:    "node",
			cmdline: `node -e process.stdin.resume(); setInterval(function(){}, 1000);`,
			want:    true,
		},
		{
			name:    "cua-driver 占位 node 的 bash 宿主壳",
			comm:    "bash",
			cmdline: `/usr/bin/bash -lic set +m; node -e "process.stdin.resume(); setInterval(function(){}, 1000);" ; echo "NODE_IDLE_DONE"`,
			want:    true,
		},
		{
			name:    "真实 node agent 子任务(无占位指纹)",
			comm:    "node",
			cmdline: `node /data/app/server.js --port 8080`,
			want:    false,
		},
		{
			name:    "普通 bash(非占位)",
			comm:    "bash",
			cmdline: `bash -c "python main.py"`,
			want:    false,
		},
		{
			name:    "copilot MainThread(不应被 hermes 规则命中)",
			comm:    "MainThread",
			cmdline: `/usr/local/lib/node_modules/@github/copilot-linux-x64/copilot`,
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isHermesInfraExec(procNode{comm: c.comm, cmdline: c.cmdline})
			if got != c.want {
				t.Errorf("isHermesInfraExec(%q,%q) = %v, want %v", c.comm, c.cmdline, got, c.want)
			}
		})
	}
}
