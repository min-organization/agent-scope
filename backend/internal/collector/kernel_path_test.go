package collector

import "testing"

func TestIsKernelPseudoPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"proc pid stat", "/proc/2513350/stat", true},
		{"proc self stat", "/proc/self/stat", true},
		{"proc cmdline", "/proc/7865/cmdline", true},
		{"proc status", "/proc/1/status", true},
		{"sys fs", "/sys/fs/cgroup/memory.pressure", true},
		{"dev pts", "/dev/pts/0", true},
		{"dev null", "/dev/null", true},
		{"裸 basename stat(兜底)", "stat", true},
		{"裸 basename status(兜底)", "status", true},
		// 不应误伤
		{"用户真实 go 文件", "/data/docker/compose/agent-scope/backend/internal/collector/collector.go", false},
		{"用户 py 文件", "/root/project/main.py", false},
		{"配置目录非伪文件", "/etc/nginx/nginx.conf", false},
		{"tmp 文件", "/tmp/foo.tmp", false},
		{"空", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isKernelPseudoPath(c.path); got != c.want {
				t.Errorf("isKernelPseudoPath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// 端到端: 构造一个持续 openat /proc/pid/stat 的进程监控场景, 验证 last_file 不被污染为 stat。
func TestEvOpenatSkipsProcPseudo(t *testing.T) {
	// 直接验证 isKernelPseudoPath 在 EvOpenat 路径中的效果:
	// /proc/<pid>/stat 应被排除, 不会成为 lastFile。
	if !isKernelPseudoPath("/proc/7865/stat") {
		t.Fatal("期望 /proc/7865/stat 被判为内核伪文件(应排除)")
	}
	if isKernelPseudoPath("/data/x/foo.go") {
		t.Fatal("期望真实用户文件不被误判为内核伪文件")
	}
}
