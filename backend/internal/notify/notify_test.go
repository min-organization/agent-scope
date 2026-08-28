package notify

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentmon/internal/config"
)

func TestNew(t *testing.T) {
	n := New(config.Config{})
	if n == nil {
		t.Fatal("New() returned nil")
	}
}

func TestSendRespectsCooldown(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "alerts.log")
	cfg := config.Config{}
	cfg.Notify.LogFile = logFile
	cfg.Notify.CooldownSeconds = 300
	n := New(cfg)

	a := Alert{PID: 123, Tool: "copilot", Kind: "llm_error", Level: "critical", Message: "test 429", Time: time.Now().Unix()}
	n.Send(a)
	n.Send(a) // second within cooldown -> suppressed

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 log line (cooldown), got %d: %s", len(lines), string(data))
	}
}

func TestSendLogFileAppend(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log.txt")
	cfg := config.Config{}
	cfg.Notify.LogFile = logFile
	n := New(cfg)

	n.Send(Alert{PID: 1, Tool: "aider", Kind: "stuck", Level: "warning", Message: "stuck 30s", Time: time.Now().Unix()})

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "stuck") {
		t.Errorf("log missing 'stuck': %s", string(data))
	}
	if !strings.Contains(string(data), "aider") {
		t.Errorf("log missing 'aider': %s", string(data))
	}
}

func TestSendConcurrentSafety(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "concurrent.log")
	cfg := config.Config{}
	cfg.Notify.LogFile = logFile
	cfg.Notify.CooldownSeconds = 1
	n := New(cfg)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			time.Sleep(time.Duration(id) * 10 * time.Millisecond)
			n.Send(Alert{PID: 100 + id, Tool: "copilot", Kind: "llm_error", Level: "critical", Message: "test", Time: time.Now().Unix()})
		}(i)
	}
	wg.Wait()

	data, _ := os.ReadFile(logFile)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Error("expected log lines, got 0")
	}
}

func TestKeyFormat(t *testing.T) {
	n := New(config.Config{})
	k := n.key(Alert{PID: 42, Kind: "stuck"})
	if k != "42:stuck" {
		t.Errorf("key = %q, want '42:stuck'", k)
	}
}
