package collector

import (
	"agentmon/internal/notify"
	"agentmon/internal/store"
	"fmt"
	"strings"
	"time"
)

func (c *Collector) detectAnomalies(m *agentMonitor, pid int, state string, needsInput bool, lastText string) {
	// 进程已退出: 跳过所有异常判定。避免对已死 pid 误报卡死/卡死残留(进程消失后 Prune 会清记录,
	// 但检测窗口内仍可能命中; 此处直接短路, 保证不向已退出 agent 发任何告警)。
	if !processExists(pid) {
		return
	}
	nowNano := time.Now().UnixNano()
	nowSec := time.Now().Unix()
	lastOut := m.lastOut.Load()
	stuckSec := int64(c.cfg.Alert.StuckSeconds)
	waitSec := int64(c.cfg.Alert.WaitSeconds)

	// 1) 等待输入未处理超时
	if needsInput {
		if m.needsInputSince == 0 {
			m.needsInputSince = nowSec
		} else if nowSec-m.needsInputSince >= waitSec {
			c.fireAlert(pid, m.tool, "wait_unhandled", "warning",
				fmt.Sprintf("等待用户输入已超过 %d 秒未处理 (状态 %s)", waitSec, state))
		}
	} else {
		// 不再等待输入 -> 自动解除陈旧的 wait_unhandled 告警(条件消失即清)
		m.needsInputSince = 0
		c.store.DeleteAlertsKind(pid, "wait_unhandled")
	}

	// 2) LLM / 运行错误(输出文本命中错误关键词)
	// 与其它告警类型(secret_leak/destructive_cmd/stuck)一致: 若无命中则自动清除
	// 已存在的 llm_error 告警(自愈), 避免错误恢复后告警永久残留。
	llmErrorHit := false
	low := strings.ToLower(lastText)
	for _, kw := range c.cfg.Alert.ErrorKeywords {
		if strings.Contains(low, strings.ToLower(kw)) {
			c.fireAlert(pid, m.tool, "llm_error", "critical",
				fmt.Sprintf("输出命中错误关键词 %q: %s", kw, truncate(lastText, 120)))
			llmErrorHit = true
			break
		}
	}
	if !llmErrorHit {
		c.store.DeleteAlertsKind(pid, "llm_error")
	}

	// 4) 凭据泄露: 命令行或写入文件名命中凭据模式(password=/token=/AKIA/.env 等)。
	// 安全侧"宁可报": agent 把密钥写进文件或带在命令行, 用户应立刻知道。
	// 读 behMu 保护的 lastCmdLine/lastEditFile, 防止与 consumeEvent 的写形成 data race。
	m.behMu.Lock()
	lastCmdLine := m.lastCmdLine
	lastEditFile := m.lastEditFile
	m.behMu.Unlock()
	cmdLow := strings.ToLower(lastCmdLine)
	fileLow := strings.ToLower(lastEditFile)
	secretHit := ""
	secretInFile := false
	for _, p := range c.cfg.Alert.SecretPatterns {
		pl := strings.ToLower(p)
		if strings.Contains(cmdLow, pl) {
			secretHit = p
			break
		}
		if strings.Contains(fileLow, pl) {
			secretHit, secretInFile = p, true
			break
		}
	}
	if secretHit != "" {
		// 绝不把命令行原文放进告警正文: 命中凭据模式即意味着其中含明文密钥, 而告警会
		// (1) 明文落 SQLite (2) POST 到 webhook —— 离开本机 (3) 写 0644 日志
		// (4) 经无鉴权 /api/alerts 暴露。任何进入 message 的内容都必须视为已公开,
		// 否则本项目"仅采集元数据"的隐私承诺就被这一条告警击穿。
		// 只上报可定位问题的元数据: 命中的模式名(来自配置, 非密钥本体)+ 命中位置 + 可执行名。
		target, where := execNameOf(lastCmdLine), "命令行"
		if secretInFile {
			// 文件名命中(如 .env / id_rsa): 文件名本身不是凭据, 可安全展示
			target, where = lastEditFile, "写入文件"
		}
		c.fireAlert(pid, m.tool, "secret_leak", "warning",
			fmt.Sprintf("疑似凭据泄露: %s 命中模式 %q(来源 %s; 凭据原文已脱敏, 不入库不外发)",
				target, secretHit, where))
	} else {
		c.store.DeleteAlertsKind(pid, "secret_leak")
	}

	// 5) 破坏性命令: 命令行命中破坏性模式(rm -rf / git push --force / DROP TABLE 等)。
	destructiveHit := ""
	for _, p := range c.cfg.Alert.DestructiveKeywords {
		if strings.Contains(cmdLow, strings.ToLower(p)) {
			destructiveHit = p
			break
		}
	}
	if destructiveHit != "" {
		// 这里必须保留命令原文 —— "rm -rf /tmp/x" 与 "rm -rf /etc" 的区别就是告警的全部价值。
		// 但同一条命令行可能同时夹带凭据, 且本告警同样会外发 webhook, 故先过 scrubSecrets
		// 掩掉敏感键的取值再上报: 命令结构可读, 密钥本体不出机器。
		c.fireAlert(pid, m.tool, "destructive_cmd", "critical",
			fmt.Sprintf("检测到破坏性命令(命中 %q): %s", destructiveHit,
				truncate(scrubSecrets(lastCmdLine, c.cfg.Alert.SecretPatterns), 120)))
	} else {
		c.store.DeleteAlertsKind(pid, "destructive_cmd")
	}

	// 3) 卡死 / 无响应: 进程存活但长时间无任何输出/活动, 且处于非健康态。
	// 注: unknown 状态已移除(后端永不产生), 故改用可达的准确条件 —— 排除健康态 idle(空闲等 prompt /
	// 会话已结束)与等待输入(由 wait_unhandled 独立告警), 仅当 running/thinking/editing/waiting 中
	// 长时间无输出时才判卡死。避免所有长时间空闲 agent 被误报"卡死"(msg 自相矛盾)。
	stuck := !needsInput && state != "idle" && state != "waiting" &&
		(nowNano-lastOut) > stuckSec*int64(time.Second)
	if stuck {
		c.fireAlert(pid, m.tool, "stuck", "critical",
			fmt.Sprintf("已 %d 秒无活动输出 (状态 %s), 疑似卡死/无响应", stuckSec, state))
	} else {
		// 脱离卡死态 -> 自动解除陈旧的 stuck 告警
		c.store.DeleteAlertsKind(pid, "stuck")
	}
}

// fireAlert 记录告警到 store 并触发主动通知。内置冷却: 同一 (pid,kind) 在
// Notify.CooldownSeconds 内只记录一次, 避免每轮 scan 重复写库刷屏。
func (c *Collector) fireAlert(pid int, tool, kind, level, message string) {
	now := time.Now().Unix()
	ck := fmt.Sprintf("%d:%s", pid, kind)
	c.alertMu.Lock()
	if prev, ok := c.lastAlert[ck]; ok && now-prev < int64(c.cfg.Notify.CooldownSeconds) {
		c.alertMu.Unlock()
		return
	}
	c.lastAlert[ck] = now
	c.alertMu.Unlock()

	ts := now
	c.store.RecordAlert(store.AlertRecord{PID: pid, Tool: tool, Kind: kind, Level: level, Message: message, TS: ts})
	if c.notifier != nil {
		c.notifier.Send(notify.Alert{PID: pid, Tool: tool, Kind: kind, Level: level, Message: message, Time: ts})
	}
}

// clearOrphanStateAlerts 解除"状态型"孤儿告警: 其绑定 pid 不在本轮活跃 agent 集合中
// (即产生告警的 agent 已退出 / 会话已归档), 但仍留在 alerts 表里 -> 自动清除。
// 之前告警仅靠 retention(默认 7 天)清理, 与 agent 生命周期脱节, 导致"进程早退出了
// 告警还挂 7 天"。注: 状态型 kind 清单在 store.DeleteOrphanStateAlerts 中定义。
func (c *Collector) clearOrphanStateAlerts(active map[int]bool) {
	// 构建活跃 pid 列表用于 SQL IN 子句
	pids := make([]int, 0, len(active))
	for pid := range active {
		pids = append(pids, pid)
	}
	c.store.DeleteOrphanStateAlerts(pids)
}

func waitingWords() []string {
	return []string{"Y/n", "yes/no", "proceed?", "allow", "[Y/n]", "confirm", "do you want", "permission", "approve"}
}

func matchAny(text string, kws []string) bool {
	for _, kw := range kws {
		if strings.Contains(text, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
