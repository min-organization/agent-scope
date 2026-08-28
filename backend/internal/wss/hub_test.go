package wss

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestHubPush 验证 hub 能将快照推送给已连接的客户端, 且断开后移除客户端。
func TestHubPush(t *testing.T) {
	hub := NewHub()
	// 用 httptest 启动一个升级端点
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 4096)
		if err != nil {
			return
		}
		hub.Add(conn)
	}))
	defer srv.Close()

	// 客户端连接
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if hub.Len() != 1 {
		t.Fatalf("期望 1 个客户端, 实际 %d", hub.Len())
	}

	// 推送一条快照
	hub.Push(Msg{Type: "snapshot", Agents: []byte(`[]`), Alerts: []byte(`[]`), Now: 123})

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if !strings.Contains(string(msg), `"type":"snapshot"`) {
		t.Fatalf("消息格式不符: %s", string(msg))
	}

	// 断开后客户端应被移除
	c.Close()
	time.Sleep(200 * time.Millisecond)
	if hub.Len() != 0 {
		t.Fatalf("断开后期望 0 客户端, 实际 %d", hub.Len())
	}
}
