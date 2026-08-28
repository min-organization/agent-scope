// Package notify 实现 agent-scope 的主动通知(异常 / 等待输入未处理)。
// 支持三种渠道: Webhook(飞书/钉钉/企微通用 inbound)、本地桌面通知(notify-send)、日志文件。
// 内置冷却: 同一 (pid,kind) 在 CooldownSeconds 内只推送一次, 防刷屏。
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"agentmon/internal/config"
)

// Alert 一条待通知的告警。
type Alert struct {
	PID     int    `json:"pid"`
	Tool    string `json:"tool"`
	Kind    string `json:"kind"`  // stuck / wait_unhandled / llm_error / proc_gone
	Level   string `json:"level"` // warning / critical
	Message string `json:"message"`
	Time    int64  `json:"time"`
}

type Notifier struct {
	cfg     config.Config
	mu      sync.Mutex
	last    map[string]int64 // key -> 上次推送 unix 秒
	webhook bool
}

func New(cfg config.Config) *Notifier {
	return &Notifier{
		cfg:  cfg,
		last: map[string]int64{},
	}
}

// key 冷却去重键
func (n *Notifier) key(a Alert) string { return fmt.Sprintf("%d:%s", a.PID, a.Kind) }

// Send 发送一条告警(内部做冷却判断)。
func (n *Notifier) Send(a Alert) {
	n.mu.Lock()
	k := n.key(a)
	now := time.Now().Unix()
	if prev, ok := n.last[k]; ok && now-prev < int64(n.cfg.Notify.CooldownSeconds) {
		n.mu.Unlock()
		return // 冷却中, 跳过
	}
	n.last[k] = now
	n.mu.Unlock()

	// 1) Webhook
	if u := n.cfg.Notify.WebhookURL; u != "" {
		n.sendWebhook(u, a)
	}
	// 2) 本地桌面通知
	if n.cfg.Notify.SystemNotify {
		n.sendDesktop(a)
	}
	// 3) 日志文件
	if f := n.cfg.Notify.LogFile; f != "" {
		n.appendLog(f, a)
	}
}

func (n *Notifier) sendWebhook(url string, a Alert) {
	// 飞书/钉钉/企微通用: 优先发 {"msg_type":"text","content":{"text":...}} 结构。
	// 若配置了 mention, 飞书可追加 @。
	text := fmt.Sprintf("[agent-scope] %s | %s (pid %d, %s)\n%s",
		a.Level, a.Kind, a.PID, a.Tool, a.Message)
	mention := n.cfg.Notify.WebhookMention
	if mention != "" {
		text += fmt.Sprintf("\n<at user_id=\"%s\"></at>", mention)
	}
	payload := map[string]interface{}{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		log.Printf("[notify] webhook 构造失败: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[notify] webhook 发送失败: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[notify] webhook 返回 %d", resp.StatusCode)
	}
}

func (n *Notifier) sendDesktop(a Alert) {
	msg := fmt.Sprintf("%s %s: %s (pid %d)", a.Level, a.Kind, a.Message, a.PID)
	// notify-send 仅在桌面会话有效; 服务器无 DISPLAY 时会失败, 忽略错误。
	cmd := exec.Command("notify-send", "agent-scope", msg)
	_ = cmd.Run()
}

func (n *Notifier) appendLog(path string, a Alert) {
	line := fmt.Sprintf("%d\t%s\t%s\t%d\t%s\t%s\n", a.Time, a.Level, a.Kind, a.PID, a.Tool, a.Message)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[notify] 日志打开失败 %s: %v", path, err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}
