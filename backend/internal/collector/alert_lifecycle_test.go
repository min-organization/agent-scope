package collector

import (
	"path/filepath"
	"testing"

	"agentmon/internal/config"
	"agentmon/internal/store"
)

// TestClearOrphanStateAlerts 验证告警生命周期绑定:
//   - 状态型(llm_error/stuck/wait_unhandled)且绑定 pid 已不在活跃集合 -> 清除
//   - 安全审计型(secret_leak/destructive_cmd) -> 始终保留(零丢失)
//   - 状态型但绑定 pid 仍活跃 -> 保留
func TestClearOrphanStateAlerts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := &Collector{store: st, cfg: config.Default(), monitors: map[string]*agentMonitor{}, pidOwner: map[int]int{}}

	// 构造数据: 活跃 claude tpid=-100(llm_error 应保留); 孤儿进程 pid=200(llm_error 应清除);
	// 孤儿 secret_leak pid=300(应保留); 孤儿 destructive_cmd pid=400(应保留)
	st.RecordAlert(store.AlertRecord{PID: -100, Tool: "claude", Kind: "llm_error", Level: "critical", Message: "活跃"})
	st.RecordAlert(store.AlertRecord{PID: 200, Tool: "claude", Kind: "llm_error", Level: "critical", Message: "孤儿"})
	st.RecordAlert(store.AlertRecord{PID: 300, Tool: "claude", Kind: "secret_leak", Level: "warning", Message: "审计"})
	st.RecordAlert(store.AlertRecord{PID: 400, Tool: "claude", Kind: "destructive_cmd", Level: "critical", Message: "审计"})

	active := map[int]bool{-100: true} // 仅 -100 活跃
	c.clearOrphanStateAlerts(active)

	alerts, err := st.RecentAlerts(1000)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]bool{}
	for _, a := range alerts {
		byKey[a.Kind+"|"+itoa(a.PID)] = true
	}
	must := func(k string) {
		if !byKey[k] {
			t.Errorf("期望保留 %s", k)
		}
	}
	mustNot := func(k string) {
		if byKey[k] {
			t.Errorf("期望已清除 %s", k)
		}
	}
	must("llm_error|-100")   // 活跃 -> 保留
	mustNot("llm_error|200") // 孤儿 -> 清除
	must("secret_leak|300")  // 审计 -> 保留
	must("destructive_cmd|400")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := [12]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
