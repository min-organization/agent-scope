package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeFakeProc 在 procRoot 下伪造 /proc/<pid>/{wchan,stat,fd/0}, 供 probeTerminalBlock 注入测试。
// wchan/stat 为空字符串表示"该文件不存在"(模拟进程刚退出); fd0 为空表示不建 stdin 符号链接。
func writeFakeProc(t *testing.T, procRoot string, pid int, wchan, stat, fd0 string) {
	t.Helper()
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(base, "fd"), 0o755); err != nil {
		t.Fatalf("建伪 proc 目录: %v", err)
	}
	if wchan != "" {
		if err := os.WriteFile(filepath.Join(base, "wchan"), []byte(wchan), 0o644); err != nil {
			t.Fatalf("写 wchan: %v", err)
		}
	}
	if stat != "" {
		if err := os.WriteFile(filepath.Join(base, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatalf("写 stat: %v", err)
		}
	}
	if fd0 != "" {
		// 符号链接目标无需真实存在(悬空链接), os.Readlink 只读链接本身。
		if err := os.Symlink(fd0, filepath.Join(base, "fd", "0")); err != nil {
			t.Fatalf("建 fd/0 链接: %v", err)
		}
	}
}

// TestProbeTerminalBlock 校验终端阻塞信号的判定。
//
// 用例中的 wchan/stat 取值来自真实机器采样(claude / node / MainThread 进程), 不是臆造:
// 内核对 epoll_wait 的 wchan 符号是 "ep_poll"(下划线), 不是 "epoll"。
//
// 历史 bug 回归点(前两个用例):
//  1. stat 的 S(可中断睡眠)曾被无条件当成"阻塞在终端等输入", 而等网络/等子进程/等定时器
//     同样是 S 态 —— 导致正在推理的 claude 被误报"等待用户输入"。S/D 现仅作反向否定。
//  2. wchan 判据曾只匹配 "epoll", 匹配不上内核真实符号 "ep_poll", 使该判据恒不生效,
//     S 态兜底成了 blocked 的唯一来源。
func TestProbeTerminalBlock(t *testing.T) {
	const (
		sleeping = "2841560 (claude) S 2841532 2841560 2841532 34817 2841560 4194304 251787"
		running  = "2841560 (claude) R 2841532 2841560 2841532 34817 2841560 4194304 251787"
	)

	cases := []struct {
		name          string
		wchan, stat   string
		fd0           string
		wantBlocked   bool
		wantInputPoll bool
	}{
		{
			// 真实采样: claude 推理中。绝不能判 blocked, 否则转录本末行是 user 时会被提升成 waiting。
			name:  "推理中的 claude(ep_poll + S + stdin 是 pts)",
			wchan: "ep_poll", stat: sleeping, fd0: "/dev/pts/1",
			wantBlocked: false, wantInputPoll: true,
		},
		{
			// 强信号: tty 专用等待函数, 确定在等键盘。
			name:  "真正阻塞在终端读取(n_tty_read)",
			wchan: "n_tty_read", stat: sleeping, fd0: "/dev/pts/1",
			wantBlocked: true, wantInputPoll: false,
		},
		{
			// 进程在跑 -> wchan 是采样残留, 一律否定。
			name:  "进程处于运行态 R(wchan 为采样残留)",
			wchan: "n_tty_read", stat: running, fd0: "/dev/pts/1",
			wantBlocked: false, wantInputPoll: false,
		},
		{
			// epoll 但 stdin 不是终端 -> 是纯网络事件循环, 与终端输入无关。
			name:  "epoll 但 stdin 指向 socket",
			wchan: "ep_poll", stat: sleeping, fd0: "socket:[123456]",
			wantBlocked: false, wantInputPoll: false,
		},
		{
			// 部分内核/运行时把符号显示为 epoll_wait, 两种拼写都要覆盖。
			name:  "epoll_wait 拼写变体",
			wchan: "epoll_wait", stat: sleeping, fd0: "/dev/pts/3",
			wantBlocked: false, wantInputPoll: true,
		},
		{
			// comm 允许含 ')' —— 系统上真实存在 comm 为 "(sd-pam)" 的进程。
			// 定位状态位必须用最后一个 ')', 用第一个会取到 ")" 而误判为非 S -> 错误否定。
			name:  "comm 含右括号(sd-pam)",
			wchan: "n_tty_read", stat: "981 ((sd-pam)) S 980 980 980 0 -1 4194368", fd0: "/dev/pts/1",
			wantBlocked: true, wantInputPoll: false,
		},
		{
			// 进程刚退出, /proc 条目消失 -> 不产生任何信号(不得残留上一轮判定)。
			name:  "进程已退出(无 wchan/stat)",
			wchan: "", stat: "", fd0: "",
			wantBlocked: false, wantInputPoll: false,
		},
		{
			// 只有 wchan 没有 stat(读 stat 竞态失败): 强信号仍成立, 无从否定。
			name:  "stat 读取失败但 wchan 命中 tty",
			wchan: "n_tty_read", stat: "", fd0: "/dev/pts/1",
			wantBlocked: true, wantInputPoll: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			const pid = 2841560
			writeFakeProc(t, root, pid, tc.wchan, tc.stat, tc.fd0)

			blocked, inputPoll := probeTerminalBlock(root, pid)
			if blocked != tc.wantBlocked || inputPoll != tc.wantInputPoll {
				t.Fatalf("期望 blocked=%v inputPoll=%v, 实际 blocked=%v inputPoll=%v",
					tc.wantBlocked, tc.wantInputPoll, blocked, inputPoll)
			}
		})
	}
}

// TestProcStatState 单独校验状态位提取(probeTerminalBlock 反向否定的依据)。
func TestProcStatState(t *testing.T) {
	cases := map[string]string{
		"2841560 (claude) S 2841532 2841560": "S",
		"2841560 (claude) R 2841532 2841560": "R",
		"981 ((sd-pam)) S 980 980":           "S", // comm 含 ')': 必须用最后一个 ')' 定位
		"1234 (my proc with spaces) D 1 1":   "D", // comm 含空格
		"":                                   "",  // 空内容
		"garbage without paren":              "",  // 无 ')' -> 无法定位
		"2841560 (claude)":                   "",  // 有 ')' 但其后无字段
	}
	for in, want := range cases {
		if got := procStatState(in); got != want {
			t.Fatalf("procStatState(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
