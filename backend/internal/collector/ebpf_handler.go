package collector

import (
	"strconv"
	"strings"
)

// refreshTrees 对所有 canonical 进程级 monitor, 重新计算进程树并把新出现的子进程加入 eBPF 监控。
func (c *Collector) refreshTrees() {
	if c.ebpfMon == nil {
		return
	}
	c.mu.Lock()
	var roots []int
	for k := range c.monitors {
		if strings.HasPrefix(k, "proc:") {
			if pid, err := strconv.Atoi(strings.TrimPrefix(k, "proc:")); err == nil {
				roots = append(roots, pid)
			}
		}
	}
	c.mu.Unlock()
	for _, root := range roots {
		dpids := descendantPids(root)
		for _, dp := range dpids {
			c.ebpfMon.AddPID(dp)
			c.mu.Lock()
			c.pidOwner[dp] = root
			c.mu.Unlock()
		}
	}
}

// pollBehavior 从 eBPF 拉取系统级行为事件, 按持久化 pidOwner(子进程->根)路由到 canonical monitor。
// pidOwner 由 refreshTrees 在子进程存活期间填充, 即使子进程已退出, 其 beh_map 事件仍可正确归属。
func (c *Collector) pollBehavior() {
	if c.ebpfMon == nil {
		return
	}
	events := c.ebpfMon.PollEvents()
	if len(events) == 0 {
		return
	}
	c.mu.Lock()
	monByPid := make(map[int]*agentMonitor)
	for k, m := range c.monitors {
		if strings.HasPrefix(k, "proc:") {
			if pid, err := strconv.Atoi(strings.TrimPrefix(k, "proc:")); err == nil {
				monByPid[pid] = m
			}
		}
	}
	owner := make(map[int]int)
	for k, v := range c.pidOwner {
		owner[k] = v
	}
	c.mu.Unlock()
	for pidU32, ev := range events {
		pid := int(pidU32)
		root, ok := owner[pid]
		if !ok {
			continue
		}
		if m, ok := monByPid[root]; ok {
			m.consumeEvent(ev, c.cfg)
		}
	}
}

func (c *Collector) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.monitors {
		if m.cancel != nil {
			m.cancel()
		}
	}
	c.monitors = make(map[string]*agentMonitor)
}
