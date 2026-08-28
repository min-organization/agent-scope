package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// CurrentConfigVersion 是当前配置 schema 版本。配置升级时递增, 旧版本由 Migrate 迁移。
const CurrentConfigVersion = 1

// Migrate 将加载的配置迁移到当前 schema 版本(干净断代, 无兼容 shim)。
// 目前 v1 即最新, 无历史版本需迁移; 未来升 v2 时在此做字段映射(旧字段 -> 新字段),
// 并递增 CurrentConfigVersion。迁移失败应返回错误(不静默忽略, 避免配置断裂)。
func (c *Config) Migrate() error {
	switch {
	case c.Version == 0:
		// 未声明版本(老配置/手写缺 version 字段): 视为 v1(当前最新)直接采用, 不报错。
		c.Version = CurrentConfigVersion
	case c.Version > CurrentConfigVersion:
		return fmt.Errorf("配置版本 %d 高于支持的版本 %d, 请升级 agent-scope", c.Version, CurrentConfigVersion)
	case c.Version < CurrentConfigVersion:
		// 未来降级/迁移入口(目前无旧版本分支)。
		c.Version = CurrentConfigVersion
	}
	return nil
}

type Config struct {
	Version int `yaml:"version"` // 配置 schema 版本(当前 1)。旧版本加载时由 Migrate 迁移。
	Server  struct {
		Addr string `yaml:"addr"`
	} `yaml:"server"`
	Collect struct {
		Interval    int      `yaml:"interval"`     // 采集间隔(秒)
		Match       []string `yaml:"match"`        // 代理可执行名关键词
		PtyRing     int      `yaml:"pty_ring"`     // pty ring buffer 行数
		IdleSeconds int      `yaml:"idle_seconds"` // 无输出静止多久判 blocked
		// wait_input_seconds: 进程阻塞在 pty 输入且 pty 无待读字节 + 零活动超过该秒数 -> 判"等待用户输入"
		WaitInputSeconds int `yaml:"wait_input_seconds"`
		// exclude: 排除列表(自身或任意祖先 cmdline/comm 命中则不作为 agent 监控)
		// 用于过滤自身会话 shell、系统监控 agent 等噪声(彻底重构: 不再污染数据)
		Exclude []string `yaml:"exclude"`
	} `yaml:"collect"`
	WaitingKeywords []string `yaml:"waiting_keywords"`
	// Behavior 行为采集(全 eBPF 采集)配置
	Behavior struct {
		// capture: off=完全不采集行为事件(不写 events 表); metadata=采集命令/文件/连接元数据(默认); full=同样仅元数据
		// 注意: 项目铁律"只采集不控制" —— 任何模式都不读取或存储 pty 原始文本字节(详见 collector.consumeEvent)。
		// 此处 capture 仅控制"是否记录 syscall 行为元数据",与隐私无关。
		Capture  string   `yaml:"capture"`
		EditExt  []string `yaml:"edit_ext"`  // 视为"编辑"的源码后缀
		LLMHosts []string `yaml:"llm_hosts"` // 视为 LLM API 的主机关键字
	} `yaml:"behavior"`
	// Store 本地存储(agent-scope.db / SQLite)保留策略。
	Store struct {
		// retain_events_sec: 行为时间线(events 表)保留秒数。默认 60(1 分钟滚动窗口)。
		// 设为 0 表示不自动清理(永久保留, 注意 DB 会增长)。
		RetainEventsSec int `yaml:"retain_events_sec"`
		// retain_alerts_days: 异常告警(alerts 表)保留天数。默认 7。设为 0 表示不自动清理。
		RetainAlertsDays int `yaml:"retain_alerts_days"`
	} `yaml:"store"`
	// Notify 主动通知渠道(异常 / 等待输入未处理时推送)
	Notify struct {
		WebhookURL     string `yaml:"webhook_url"`     // 飞书/钉钉/企微通用 inbound webhook
		WebhookMention string `yaml:"webhook_mention"` // 可选 @某人(飞书 open_id/手机号)
		SystemNotify   bool   `yaml:"system_notify"`   // 本地桌面通知(notify-send), 仅桌面环境有效
		LogFile        string `yaml:"log_file"`        // 异常日志文件(外部监控可抓取), 空=不写
		// 通知冷却: 同一 (pid,kind) 至少间隔该秒数再推送, 防刷屏
		CooldownSeconds int `yaml:"cooldown_seconds"`
	} `yaml:"notify"`
	// Alert 异常检测阈值
	Alert struct {
		// stuck_seconds: 非活动(非 running/thinking/editing/waiting)且超过该秒数 -> 判"卡死/无响应"
		StuckSeconds int `yaml:"stuck_seconds"`
		// wait_seconds: needs_input 持续超过该秒数未处理 -> 判"长时间等待输入未处理"
		WaitSeconds int `yaml:"wait_seconds"`
		// error_keywords: 输出文本命中这些词 -> 判 LLM/运行错误(如 429/timeout/rate limit)
		ErrorKeywords []string `yaml:"error_keywords"`
		// destructive_keywords: 命令行命中这些子串(大小写不敏感) -> 判破坏性命令(如 rm -rf / git push --force)
		DestructiveKeywords []string `yaml:"destructive_keywords"`
		// secret_patterns: 命令行或写入文件名命中这些子串(大小写不敏感) -> 判凭据泄露(如 password= / AKIAxxx / .env)
		SecretPatterns []string `yaml:"secret_patterns"`
	} `yaml:"alert"`
}

func Default() *Config {
	c := &Config{}
	c.Version = CurrentConfigVersion
	c.Server.Addr = ":8090"
	c.Collect.Interval = 2
	c.Collect.Match = []string{"claude", "codex", "copilot", "aider", "opencode", "gemini", "hermes", "openclaw"}
	c.Collect.Exclude = []string{
		"agent-scope",   // 本项目自身进程
		"elkeid",        // 系统安全 agent(非 AI coding agent)
		"cloud-monitor", // 系统监控 agent
	}
	c.Collect.PtyRing = 200
	c.Collect.IdleSeconds = 5
	c.Collect.WaitInputSeconds = 8
	c.Notify.CooldownSeconds = 300
	c.Alert.StuckSeconds = 120
	c.Alert.WaitSeconds = 60
	c.Alert.ErrorKeywords = []string{
		"429", "rate limit", "rate_limit", "timeout", "timed out", "connection refused",
		"ECONNREFUSED", "ETIMEDOUT", "quota", "exceeded", "unauthorized",
		"401", "403", "500", "502", "503", "panic:", "fatal", "OOM", "out of memory",
	}
	c.Alert.DestructiveKeywords = []string{
		"rm -rf", "rm -fr", "git push --force", "git push -f", "git reset --hard",
		"DROP TABLE", "DROP DATABASE", "mkfs", "chmod -R 777", "truncate -s 0",
		":(){", "dd if=",
	}
	c.Alert.SecretPatterns = []string{
		"password=", "passwd=", "pwd=", "token=", "secret=", "api_key=", "apikey=",
		"AKIA", "Bearer ", "Authorization: Bearer", ".env", "credentials", "id_rsa",
		".pem", ".key",
	}
	c.WaitingKeywords = []string{
		"Y/n", "yes/no", "Proceed?", "Allow", "[Y/n]", "confirm", "⏎",
		"do you want", "permission", "approve",
	}
	c.Behavior.Capture = "full" // 全面可观测: 完整 pty 输出 + 行为元数据(用户已授权放开隐私闸门)
	c.Behavior.EditExt = []string{".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs", ".java", ".c", ".cpp", ".h", ".rb", ".php", ".vue", ".md"}
	c.Behavior.LLMHosts = []string{
		"openai.com", "anthropic.com", "copilot", "codex", "googleapis.com",
		"ai.google", "deepseek.com", "moonshot", "qwen", "baidu.com",
	}
	c.Store.RetainEventsSec = 60 // 行为时间线默认保留 1 分钟
	c.Store.RetainAlertsDays = 7 // 异常告警默认保留 7 天
	return c
}

func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		// 尝试默认位置
		path = "agent-scope.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil // 用默认配置
		}
		return nil, fmt.Errorf("读配置 %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	// 配置版本迁移(旧版本 -> 当前 schema, 干净断代无 shim)。
	if err := c.Migrate(); err != nil {
		return nil, err
	}
	// 严格模式: 用 Decoder.KnownFields(true) 检测未知字段(配置 typo 保护)
	if err := yamlKnownFieldsCheck(string(data)); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	if len(c.Collect.Match) == 0 {
		return nil, fmt.Errorf("collect.match 不能为空, 请配置至少一个 agent 可执行名关键词")
	}
	if c.Collect.Interval <= 0 {
		c.Collect.Interval = 2
	}
	if c.Collect.IdleSeconds <= 0 {
		c.Collect.IdleSeconds = 5
	}
	if c.Collect.PtyRing <= 0 {
		c.Collect.PtyRing = 200
	}
	if c.Store.RetainEventsSec < 0 {
		c.Store.RetainEventsSec = 0
	}
	if c.Store.RetainAlertsDays < 0 {
		c.Store.RetainAlertsDays = 0
	}
	return c, nil
}

// yamlKnownFieldsCheck 对配置做"已知字段"严格校验, 防止用户 typo 导致配置静默忽略。
func yamlKnownFieldsCheck(raw string) error {
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	var dummy Config
	if err := dec.Decode(&dummy); err != nil {
		return fmt.Errorf("未知字段: %w", err)
	}
	return nil
}
