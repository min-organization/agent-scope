package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agentmon/internal/collector"
	"agentmon/internal/config"
	"agentmon/internal/ebpf"
	"agentmon/internal/notify"
	"agentmon/internal/server"
	"agentmon/internal/store"
	"agentmon/internal/wss"
)

func main() {
	addr := flag.String("addr", "", "监听地址, 覆盖配置")
	configPath := flag.String("config", "agent-scope.yaml", "配置文件路径")
	dbPath := flag.String("db", "data/agent-scope.db", "SQLite 路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		os.Exit(1)
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "存储错误: %v\n", err)
		os.Exit(1)
	}

	// WebSocket 实时推送 hub(只推送观测数据, 不接收控制指令)
	hub := wss.NewHub()

	// 尝试加载 eBPF(零竞争抓 pty 输出); 失败则降级为 pty 读取
	var ebpfMon *ebpf.Monitor
	if m, err := ebpf.New(); err != nil {
		fmt.Fprintf(os.Stderr, "eBPF 不可用(降级 pty 读取): %v\n", err)
	} else {
		ebpfMon = m
		defer ebpfMon.Close()
	}

	col := collector.New(cfg, st, notify.New(*cfg), hub, ebpfMon)
	ctx, cancel := context.WithCancel(context.Background())
	// collectorDone 在 col.Run 真正返回后关闭。store 必须等它之后再关 —— col.Run 收到
	// ctx.Done 还会执行一轮 shutdown, 且可能正处于 scan() 中途写库; 用 defer st.Close()
	// 会在 main 返回时立刻关闭连接, 与采集协程形成竞态("database is closed")。
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		col.Run(ctx)
	}()

	srv := server.New(st, hub, cfg.Server.Addr)
	go func() {
		fmt.Printf("agent-scope 监听 %s\n", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			fmt.Fprintf(os.Stderr, "HTTP 错误: %v\n", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\n正在关闭...")
	cancel()
	shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	srv.Shutdown(shutdownCtx)
	// 等采集协程退出后才关 store(见上方 collectorDone 注释)。超时则放弃等待继续关闭,
	// 避免采集协程卡住导致进程无法退出。
	select {
	case <-collectorDone:
	case <-time.After(3 * time.Second):
		fmt.Fprintln(os.Stderr, "采集协程未在 3 秒内退出, 强制关闭存储")
	}
	if err := st.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "关闭存储: %v\n", err)
	}
}
