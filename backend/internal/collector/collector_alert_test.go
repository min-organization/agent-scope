package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentmon/internal/config"
	"agentmon/internal/notify"
	"agentmon/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// 构造带极低阈值的配置, 让检测快速命中
func alertCfg() *config.Config {
	c := config.Default()
	c.Alert.StuckSeconds = 1
	c.Alert.WaitSeconds = 1
	c.Alert.ErrorKeywords = []string{"429", "panic:", "fatal"}
	c.Notify.CooldownSeconds = 1
	// 日志文件用于断言 notifier 确实被调用
	dir, _ := os.MkdirTemp("", "am-alert")
	c.Notify.LogFile = filepath.Join(dir, "alerts.log")
	return c
}

func TestDetectAnomaliesStuck(t *testing.T) {
	cfg := alertCfg()
	st := newTestStore(t)
	nt := notify.New(*cfg)
	c := &Collector{cfg: cfg, store: st, notifier: nt, lastAlert: map[string]int64{}}

	// idle(正常空闲)即使长时间无输出也不应报卡死 —— 否则 copilot 开着不对话会被误报"卡死/无响应"
	m := &agentMonitor{tool: "node", pid: os.Getpid()}
	m.lastOut.Store(time.Now().Add(-10 * time.Second).UnixNano()) // 10s 前输出 -> 远超 stuck 阈值
	c.detectAnomalies(m, os.Getpid(), "idle", false, "(无输出)")
	alerts, _ := st.RecentAlerts(10)
	if len(alerts) != 0 {
		t.Fatalf("idle 不应触发 stuck, 实际 %+v", alerts)
	}

	// 卡死候选: 非健康态(running/thinking/editing)长时间无输出 -> 应报 stuck
	// 注: unknown 状态已移除(后端永不产生), stuck 现触发于 state!=idle && state!=waiting 的长时间静默。
	for _, stState := range []string{"running", "thinking", "editing"} {
		st2 := newTestStore(t)
		c2 := &Collector{cfg: cfg, store: st2, notifier: nt, lastAlert: map[string]int64{}}
		m2 := &agentMonitor{tool: "node", pid: os.Getpid()}
		m2.lastOut.Store(time.Now().Add(-10 * time.Second).UnixNano())
		c2.detectAnomalies(m2, os.Getpid(), stState, false, "(无输出)")
		a2, _ := st2.RecentAlerts(10)
		if len(a2) != 1 || a2[0].Kind != "stuck" || a2[0].Level != "critical" {
			t.Fatalf("状态 %s 期望 stuck/critical, 实际 %+v", stState, a2)
		}
	}

	// waiting 不触发 stuck(由 wait_unhandled 独立告警)
	st3 := newTestStore(t)
	c3 := &Collector{cfg: cfg, store: st3, notifier: nt, lastAlert: map[string]int64{}}
	m3 := &agentMonitor{tool: "node", pid: os.Getpid()}
	m3.lastOut.Store(time.Now().Add(-10 * time.Second).UnixNano())
	c3.detectAnomalies(m3, os.Getpid(), "waiting", false, "(无输出)")
	if a3, _ := st3.RecentAlerts(10); len(a3) != 0 {
		t.Fatalf("waiting 不应触发 stuck, 实际 %+v", a3)
	}
}

func TestDetectAnomaliesWaitUnhandled(t *testing.T) {
	cfg := alertCfg()
	st := newTestStore(t)
	nt := notify.New(*cfg)
	c := &Collector{cfg: cfg, store: st, notifier: nt, lastAlert: map[string]int64{}}

	m := &agentMonitor{tool: "copilot", pid: os.Getpid()}
	// 模拟 needs_input 已持续很久(needsInputSince 很久以前)
	m.needsInputSince = time.Now().Add(-60 * time.Second).Unix()
	c.detectAnomalies(m, os.Getpid(), "waiting", true, "(无输出)")

	alerts, _ := st.RecentAlerts(10)
	if len(alerts) != 1 || alerts[0].Kind != "wait_unhandled" {
		t.Fatalf("期望 wait_unhandled 告警, 实际 %+v", alerts)
	}
}

func TestDetectAnomaliesLLMError(t *testing.T) {
	cfg := alertCfg()
	st := newTestStore(t)
	nt := notify.New(*cfg)
	c := &Collector{cfg: cfg, store: st, notifier: nt, lastAlert: map[string]int64{}}

	m := &agentMonitor{tool: "copilot", pid: os.Getpid()}
	m.lastOut.Store(time.Now().UnixNano())
	// 输出命中错误关键词 -> llm_error
	c.detectAnomalies(m, os.Getpid(), "editing", false, "Error: 429 Too Many Requests from api.openai.com")
	// 冷却后再来一次, 应被 cooldown 抑制(同一 pid+kind)
	c.detectAnomalies(m, os.Getpid(), "editing", false, "Error: 429 Too Many Requests again")

	alerts, _ := st.RecentAlerts(10)
	if len(alerts) != 1 || alerts[0].Kind != "llm_error" {
		t.Fatalf("期望仅 1 条 llm_error(冷却抑制重复), 实际 %+v", alerts)
	}
	// 断言 notifier 日志文件确有写入(证明通知渠道真的触发了)
	data, err := os.ReadFile(cfg.Notify.LogFile)
	if err != nil || !strings.Contains(string(data), "llm_error") {
		t.Fatalf("notifier 日志未写入 llm_error: err=%v content=%q", err, string(data))
	}
}

// TestDetectAnomalies_SubagentNoAlert 回归: 树重构后, 子进程节点(脚本/shell 壳)即使 waiting,
// 也不应独立产生 wait_unhandled 告警(否则一个等待输入的 root 会被 4 个节点刷成 N 倍告警)。
// 验证 scan 层的契约: 仅 depth==0 的 root 才调用 detectAnomalies。
func TestDetectAnomalies_SubagentNoAlert(t *testing.T) {
	cfg := alertCfg()
	st := newTestStore(t)
	nt := notify.New(*cfg)
	c := &Collector{cfg: cfg, store: st, notifier: nt, lastAlert: map[string]int64{}}

	// 模拟 copilot 根 + 三个子进程全部 needs_input(真实等待输入场景)
	root := &agentMonitor{tool: "copilot", pid: os.Getpid()}
	root.needsInputSince = time.Now().Add(-120 * time.Second).Unix()
	children := []*agentMonitor{
		{tool: "script", pid: 2211962, needsInputSince: time.Now().Add(-120 * time.Second).Unix()},
		{tool: "node", pid: 2211963, needsInputSince: time.Now().Add(-120 * time.Second).Unix()},
		{tool: "MainThread", pid: 2211975, needsInputSince: time.Now().Add(-120 * time.Second).Unix()},
	}

	// 复刻 scan 的调用契约: 只 depth==0 (root) 调 detectAnomalies
	c.detectAnomalies(root, os.Getpid(), "waiting", true, "(无输出)")
	for _, ch := range children {
		// depth>0 的子节点不调用 —— 这正是修复点
		_ = ch
	}

	alerts, _ := st.RecentAlerts(20)
	waitAlerts := 0
	for _, a := range alerts {
		if a.Kind == "wait_unhandled" {
			waitAlerts++
		}
	}
	if waitAlerts != 1 {
		t.Fatalf("期望仅 root 产生 1 条 wait_unhandled(子节点不告警), 实际 %d: %+v", waitAlerts, alerts)
	}
}

// TestDetectAnomalies_DeadPIDNoAlert 回归: 进程已退出的 pid 不应触发任何告警(避免对已死 agent 误报卡死)。
func TestDetectAnomalies_DeadPIDNoAlert(t *testing.T) {
	cfg := alertCfg()
	st := newTestStore(t)
	nt := notify.New(*cfg)
	c := &Collector{cfg: cfg, store: st, notifier: nt, lastAlert: map[string]int64{}}

	// 用一个不存在的 pid(远大于系统范围), 即使状态是 idle + 无输出, 也不应告警
	deadPID := 999999999
	if processExists(deadPID) {
		t.Skip("恰巧存在该 pid, 跳过")
	}
	m := &agentMonitor{tool: "copilot", pid: deadPID}
	m.lastOut.Store(0) // 很久无输出
	c.detectAnomalies(m, deadPID, "idle", false, "(无输出)")

	alerts, _ := st.RecentAlerts(20)
	if len(alerts) != 0 {
		t.Fatalf("已退出的 pid 不应产生任何告警, 实际 %+v", alerts)
	}
}
