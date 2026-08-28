package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func tmpDB(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	st, err := Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close(); os.Remove(p) })
	return st
}

func TestEventRecordAndRecent(t *testing.T) {
	st := tmpDB(t)
	for i := 0; i < 5; i++ {
		if err := st.RecordEvent(Event{PID: 100, Ts: int64(i), Kind: "state", Detail: "running", State: "running"}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	evs, err := st.RecentEvents(100, 3, false)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("期望 3 条, 得 %d", len(evs))
	}
	// 应为时间正序(偏移 2,3,4)
	if evs[0].Ts != 2 || evs[2].Ts != 4 {
		t.Fatalf("顺序错误: %+v", evs)
	}
	// 其他 pid 不应返回
	other, err := st.RecentEvents(999, 10, false)
	if err != nil {
		t.Fatalf("recent other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("期望 0 条, 得 %d", len(other))
	}
}

func TestPruneEvents(t *testing.T) {
	st := tmpDB(t)
	_ = st.RecordEvent(Event{PID: 1, Ts: 10, Kind: "cmd", Detail: "x"})
	_ = st.RecordEvent(Event{PID: 1, Ts: 20, Kind: "cmd", Detail: "y"})
	// 删除 ts<15 的, 仅留 20
	if err := st.PruneEvents(15); err != nil {
		t.Fatalf("prune: %v", err)
	}
	evs, _ := st.RecentEvents(1, 10, false)
	if len(evs) != 1 || evs[0].Ts != 20 {
		t.Fatalf("prune 失败: %+v", evs)
	}
}

// TestOpen_MigrateStaleSchema 回归: 旧版(v1.7-) DB 无 root_pid 等新列时,
// 重新 Open 应自动 DROP 重建(清空重采无碍), 不报 "no such column"。
// TestDeleteAlertsKind 验证状态告警自动解除: 删除指定 pid+kind 后不再出现。
func TestDeleteAlertsKind(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "agent-scope.db")
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.RecordAlert(AlertRecord{PID: 100, Tool: "copilot", Kind: "wait_unhandled", Message: "x", TS: 1})
	st.RecordAlert(AlertRecord{PID: 100, Tool: "copilot", Kind: "stuck", Message: "y", TS: 2})
	st.RecordAlert(AlertRecord{PID: 200, Tool: "copilot", Kind: "wait_unhandled", Message: "z", TS: 3})
	if err := st.DeleteAlertsKind(100, "wait_unhandled"); err != nil {
		t.Fatal(err)
	}
	// 100 的 wait_unhandled 已删, stuck 仍在; 200 的不受影响
	all, _ := st.RecentAlerts(50)
	left := map[string]bool{}
	for _, a := range all {
		left[fmt.Sprintf("%d:%s", a.PID, a.Kind)] = true
	}
	if left["100:wait_unhandled"] {
		t.Error("100:wait_unhandled 应已删除")
	}
	if !left["100:stuck"] {
		t.Error("100:stuck 不应被删")
	}
	if !left["200:wait_unhandled"] {
		t.Error("200:wait_unhandled 不应被删")
	}
}

func TestUpsertRejectsInvalidState(t *testing.T) {
	st := tmpDB(t)
	if err := st.Upsert(Agent{PID: 1, Tool: "claude", State: "blocked"}); err == nil {
		t.Fatal("invalid state should be rejected")
	}
}

func TestConcurrentUpsert(t *testing.T) {
	st := tmpDB(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(pid int) {
			defer wg.Done()
			_ = st.Upsert(Agent{PID: pid, Tool: "copilot", State: "running"})
		}(100 + i)
	}
	wg.Wait()
	all, err := st.List()
	if err != nil {
		t.Fatalf("List after concurrent Upsert: %v", err)
	}
	if len(all) != 10 {
		t.Errorf("expected 10 agents after concurrent upsert, got %d", len(all))
	}
}

func TestConcurrentUpsertSameKey(t *testing.T) {
	st := tmpDB(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.Upsert(Agent{PID: 1, Tool: "copilot", State: "idle"})
		}()
	}
	wg.Wait()
	// Should have exactly 1 agent
	all, _ := st.List()
	if len(all) != 1 {
		t.Errorf("expected 1 agent after concurrent upsert of same key, got %d", len(all))
	}
}

func TestConcurrentListAndUpsert(t *testing.T) {
	st := tmpDB(t)
	_ = st.Upsert(Agent{PID: 1, Tool: "copilot", State: "running"})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = st.List()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.Upsert(Agent{PID: 1, Tool: "copilot", State: "idle"})
		}()
	}
	wg.Wait()
}

func TestPruneAlerts(t *testing.T) {
	st := tmpDB(t)
	_ = st.RecordAlert(AlertRecord{PID: 1, Kind: "stuck", TS: 100})
	_ = st.RecordAlert(AlertRecord{PID: 1, Kind: "stuck", TS: 200})
	_ = st.RecordAlert(AlertRecord{PID: 2, Kind: "stuck", TS: 300})

	// Prune alerts with ts < 250
	if err := st.PruneAlerts(250); err != nil {
		t.Fatalf("PruneAlerts: %v", err)
	}
	all, _ := st.RecentAlerts(10)
	if len(all) != 1 {
		t.Errorf("expected 1 alert after prune, got %d", len(all))
	}
	if all[0].TS != 300 {
		t.Errorf("remaining alert should be at ts 300, got %d", all[0].TS)
	}
}

func TestOpen_MigrateStaleSchema(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "agent-scope.db")
	st0, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	// 强行改写成旧 schema(无 root_pid 等新列)
	if _, err := st0.db.Exec(`DROP TABLE agents`); err != nil {
		t.Fatal(err)
	}
	oldDDL := `CREATE TABLE agents (pid INTEGER PRIMARY KEY, tool TEXT, state TEXT, confidence TEXT, last_text TEXT, updated_at INTEGER, last_cmd TEXT, last_file TEXT, last_conn TEXT, state_detail TEXT, needs_input INTEGER DEFAULT 0)`
	if _, err := st0.db.Exec(oldDDL); err != nil {
		t.Fatal(err)
	}
	st0.Close()

	st, err := Open(db)
	if err != nil {
		t.Fatalf("重新 Open 旧 schema DB 应自动迁移, 实际报错: %v", err)
	}
	defer st.Close()
	if err := st.Upsert(Agent{PID: 1, Tool: "copilot", State: "running", ParentPID: 0, RootPID: 1, Depth: 0, IsSubagent: false, Task: "x", Src: "proc"}); err != nil {
		t.Fatalf("迁移后 Upsert 失败: %v", err)
	}
	if _, err := st.ListTree(); err != nil {
		t.Fatalf("迁移后 ListTree 失败: %v", err)
	}
}
