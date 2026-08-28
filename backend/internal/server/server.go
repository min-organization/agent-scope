package server

import (
	"embed"
	"encoding/json"
	"io/fs"

	"net/http"
	"strconv"
	"strings"
	"time"

	"agentmon/internal/store"
	"agentmon/internal/wss"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

//go:embed all:web
var webFS embed.FS

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		// 内网监控: 只接受本机连接
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
			strings.HasPrefix(host, "127.") ||
			strings.HasPrefix(r.RemoteAddr, "127.") ||
			r.RemoteAddr == "@" || r.RemoteAddr == ""
	},
}

func New(st *store.Store, hub *wss.Hub, addr string) *http.Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.GET("/api/agents", func(c *gin.Context) {
		agents, err := st.ListTree()
		if err != nil {
			c.Error(err)
			c.JSON(500, gin.H{"error": "internal error"})
			return
		}
		c.JSON(200, agents)
	})
	r.GET("/api/events", func(c *gin.Context) {
		pid, _ := strconv.Atoi(c.Query("pid"))
		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 {
			limit = 50
		}
		onlyUser := c.Query("only_user") != "0"
		evs, err := st.RecentEvents(pid, limit, onlyUser)
		if err != nil {
			c.Error(err)
			c.JSON(500, gin.H{"error": "internal error"})
			return
		}
		c.JSON(200, evs)
	})
	r.GET("/api/alerts", func(c *gin.Context) {
		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 {
			limit = 200
		}
		alerts, err := st.RecentAlerts(limit)
		if err != nil {
			c.Error(err)
			c.JSON(500, gin.H{"error": "internal error"})
			return
		}
		c.JSON(200, alerts)
	})

	// WebSocket 实时推送(只推送观测数据, 不接收控制指令)
	r.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		hub.Add(conn)
		// 立即推送一次当前快照
		if agents, err := st.ListTree(); err == nil {
			al, _ := st.RecentAlerts(50)
			hub.Push(snapshot(agents, al))
		}
	})

	sub, err := fs.Sub(webFS, "web/dist")
	if err == nil {
		fileServer := http.FileServer(http.FS(sub))
		r.NoRoute(func(c *gin.Context) {
			fileServer.ServeHTTP(c.Writer, c.Request)
		})
	}

	return &http.Server{Addr: addr, Handler: r}
}

// snapshot 构造一条全量推送消息。
func snapshot(agents []store.Agent, alerts []store.AlertOut) wss.Msg {
	a, _ := json.Marshal(agents)
	b, _ := json.Marshal(alerts)
	return wss.Msg{Type: "snapshot", Agents: a, Alerts: b, Now: time.Now().Unix()}
}
