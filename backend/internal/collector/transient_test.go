package collector

import "testing"

func TestIsTransientFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// 真实业务文件 -> 非临时
		{"fib.py", false},
		{"main.go", false},
		{"README.md", false},
		{"src/utils.ts", false},
		{"config.yaml", false},
		// 明确临时/内部后缀
		{"foo.tmp", true},
		{"foo.lock", true},
		{"foo.swp", true},
		{"foo.pyc", true},
		{"foo.bak", true},
		// agent 内部随机名
		{"hermes-snap-063fcdf3a5a0.sh.tmp.MDv9CkeNf9", true},
		{"exp-cache.json.tmp.1e5cb45d-395c-494c-89a4", true},
		{"105ebccf-f506-4a1e-b05a-f8d8a509a7aa", true},
		{"drain_request.json", true},
		// 随机后缀(全字母数字 >=6) -> 临时
		{"script.sh.tmp.aZ3xK9", true},
		// 正常扩展名(非全随机后缀) -> 非临时
		{"data.json", false},
		{"output.txt", false},
	}
	for _, c := range cases {
		if got := isTransientFile(c.name); got != c.want {
			t.Errorf("isTransientFile(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
