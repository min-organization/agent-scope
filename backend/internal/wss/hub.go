// Package wss 实现 agent-scope 的 WebSocket 实时推送。
// 仅推送观测数据(agent 状态 / 告警), 不接收任何控制指令(项目定位: 只观测、不控制)。
package wss

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Msg 推送消息。type=snapshot 为全量状态 + 告警快照。
type Msg struct {
	Type   string          `json:"type"` // snapshot
	Agents json.RawMessage `json:"agents,omitempty"`
	Alerts json.RawMessage `json:"alerts,omitempty"`
	Now    int64           `json:"now"`
}

// Hub 管理所有 WS 客户端连接, 由 Collector 调 Push 广播。
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
}

type client struct {
	conn *websocket.Conn
}

// NewHub 创建空 hub。
func NewHub() *Hub {
	return &Hub{clients: map[*client]struct{}{}}
}

// Add 注册一个已升级的 WS 连接, 并启动读循环(仅用于检测断线, 不处理任何入站指令)。
func (h *Hub) Add(conn *websocket.Conn) {
	c := &client{conn: conn}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	n := len(h.clients)
	h.mu.Unlock()
	log.Printf("[ws] 客户端接入, 当前 %d", n)
	go func() {
		defer h.remove(c)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return // 断线或收到任何帧都退出(不解析指令)
			}
		}
	}()
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.clients, c)
	n := len(h.clients)
	h.mu.Unlock()
	_ = c.conn.Close()
	log.Printf("[ws] 客户端断开, 当前 %d", n)
}

// Push 广播一条消息给所有客户端。写失败的连接会被移除。
func (h *Hub) Push(m Msg) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			_ = c.conn.Close()
			delete(h.clients, c)
		}
	}
}

// Len 当前连接数。
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
