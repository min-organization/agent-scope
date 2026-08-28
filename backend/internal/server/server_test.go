package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"agentmon/internal/store"
	"agentmon/internal/wss"
)

func TestHealthz(t *testing.T) {
	st := openTestStore(t)
	hub := wss.NewHub()
	srv := New(st, hub, ":0")

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("GET /healthz status = %d, want 200", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want 'ok'", body["status"])
	}
}

func TestAgentsAPI(t *testing.T) {
	st := openTestStore(t)
	hub := wss.NewHub()

	// Insert a test agent
	_ = st.Upsert(store.Agent{PID: 42, Tool: "copilot", State: "idle"})

	srv := New(st, hub, ":0")
	req := httptest.NewRequest("GET", "/api/agents", nil)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("GET /api/agents status = %d, want 200", w.Code)
	}
	var agents []store.Agent
	if err := json.Unmarshal(w.Body.Bytes(), &agents); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(agents) != 1 || agents[0].PID != 42 {
		t.Errorf("expected 1 agent with PID 42, got %+v", agents)
	}
}

func TestAlertsAPI(t *testing.T) {
	st := openTestStore(t)
	hub := wss.NewHub()

	st.RecordAlert(store.AlertRecord{PID: 1, Tool: "copilot", Kind: "stuck", Level: "critical", Message: "testalert"})

	srv := New(st, hub, ":0")
	req := httptest.NewRequest("GET", "/api/alerts?limit=10", nil)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("GET /api/alerts status = %d, want 200", w.Code)
	}
	var alerts []store.AlertOut
	if err := json.Unmarshal(w.Body.Bytes(), &alerts); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(alerts) != 1 {
		t.Errorf("expected 1 alert, got %d", len(alerts))
	}
	if alerts[0].Kind != "stuck" {
		t.Errorf("kind = %q, want 'stuck'", alerts[0].Kind)
	}
}

func TestEventsAPI(t *testing.T) {
	st := openTestStore(t)
	hub := wss.NewHub()

	st.RecordEvent(store.Event{PID: 1, Kind: "cmd", Detail: "go build"})

	srv := New(st, hub, ":0")
	req := httptest.NewRequest("GET", "/api/events?pid=1&limit=10", nil)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("GET /api/events status = %d, want 200", w.Code)
	}
	var evs []store.Event
	if err := json.Unmarshal(w.Body.Bytes(), &evs); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(evs) != 1 {
		t.Errorf("expected 1 event, got %d", len(evs))
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
