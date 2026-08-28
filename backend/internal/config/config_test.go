package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c == nil {
		t.Fatal("Default() returned nil")
	}
	if c.Server.Addr != ":8090" {
		t.Errorf("Default addr = %q, want :8090", c.Server.Addr)
	}
	if c.Collect.Interval != 2 {
		t.Errorf("Default interval = %d, want 2", c.Collect.Interval)
	}
	if c.Collect.PtyRing != 200 {
		t.Errorf("Default pty_ring = %d, want 200", c.Collect.PtyRing)
	}
	if c.Collect.IdleSeconds != 5 {
		t.Errorf("Default idle_seconds = %d, want 5", c.Collect.IdleSeconds)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	c, err := Load("/nonexistent/path/agent-scope.yaml")
	if err != nil {
		t.Fatalf("Load with nonexistent file should return default, got error: %v", err)
	}
	if c == nil {
		t.Fatal("Load returned nil config")
	}
	if c.Server.Addr != ":8090" {
		t.Errorf("Expected default addr :8090, got %s", c.Server.Addr)
	}
}

func TestLoadValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	content := []byte("server:\n  addr: \":9999\"\ncollect:\n  interval: 5\n  pty_ring: 100\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v", path, err)
	}
	if c.Server.Addr != ":9999" {
		t.Errorf("addr = %q, want :9999", c.Server.Addr)
	}
	if c.Collect.Interval != 5 {
		t.Errorf("interval = %d, want 5", c.Collect.Interval)
	}
	if c.Collect.PtyRing != 100 {
		t.Errorf("pty_ring = %d, want 100", c.Collect.PtyRing)
	}
}

func TestLoadIntervalValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zero.yaml")
	content := []byte("collect:\n  interval: 0\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v", path, err)
	}
	if c.Collect.Interval != 2 {
		t.Errorf("interval after zero = %d, want 2", c.Collect.Interval)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("{{invalid yaml}}\n"), 0644); err != nil {
		t.Fatalf("write file %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadEmptyMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty_match.yaml")
	content := []byte("collect:\n  match: []\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with empty match should return error")
	}
}
