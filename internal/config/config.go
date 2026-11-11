// Package config 负责"配置从哪来、优先谁、来源是什么"。
//
// 依据《学习.md》10.5：ENV > 文件 > 默认，并且要返回来源供启动日志；
// 数据目录一律转绝对路径（相对路径在换 CWD 后会丢数据）。
package config

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Source 标记一个值是从哪一层读到的，启动日志会打印它。
type Source string

const (
	SourceEnv     Source = "env"
	SourceFile    Source = "file:.env.local"
	SourceDefault Source = "default"
)

// Config 是进程级配置（任务级配置在 DB 里，不在这里）。
type Config struct {
	DSN       string
	DSNOrigin Source

	DataDir  string
	Addr     string
	LogLevel string

	// AllowLoopback 允许 HTTP 工具访问回环地址。
	// 默认 false：这是 SSRF/自伤防护的一部分；本地 demo 显式打开。
	AllowLoopback bool

	MaxToolOutput   int           // 单次工具输出上限（字符），超过落盘
	DefaultMaxTurns int           // worker 每次执行的回合上限
	DefaultTimeout  time.Duration // worker 每次执行的时长上限
	// MaxUploadMB 是单个附件上限（M12）。
	MaxUploadMB int
	// DefaultTaskTimeout 是建任务时没给 timeout 的默认墙钟（秒；0 = 不限）。
	DefaultTaskTimeout int

	// M3.5：调度与上下文成本的可调项（都有安全默认值，不改也能跑）。
	GoalsMaxTurns    int // 第 0 轮目标拆解的回合上限
	PlannerFullEvery int // planner 每 K 轮给一次全量态势
	DeltaLimit       int // 单轮增量最多多少条变化（超过退全量）

	// M4：长跑可靠性。
	ContextWindowK    int // 上下文窗口（K tokens）：压缩用的估计值；0 = 关闭压缩
	SettlementTurns   int // 撞预算后的收尾轮数（0 = 关闭）
	SettlementTimeout time.Duration
	MaxConcurrentRuns int // 同时跑的任务上限（0 = 不限）

	// M6a：审批门。
	ApprovalTimeout   time.Duration // 等人审批的上限
	ApprovalOnTimeout string        // deny（默认）| allow
	ApprovalDisabled  bool          // true = 完全关闭审批门（默认 false）

	// P3-4：按风险分级的等待与超时动作（低/中/高）。
	// 语义：低风险等到点自动放行；中/高风险到点拒绝并留痕（夜间不卡死，也不放开闸门）。
	ApprovalLowSeconds  int
	ApprovalMidSeconds  int
	ApprovalHighSeconds int
	ApprovalLowAction   string
	ApprovalMidAction   string
	ApprovalHighAction  string

	// P3-3：外部连接器（空 = 不启用，行为与升级前一致）。
	Connectors ConnectorConfig

	// M9：HTTP 鉴权（空令牌 + 回环监听 = 开发态；监听非回环必须有令牌）。
	Auth AuthConfig

	LLM LLMConfig

	// M17：执行沙箱（默认关闭）。开启后 worker 多一个 exec 工具，
	// 出网只按 run_scope + 回连白名单放行（docs/24）。
	Sandbox SandboxConfig
}

// SandboxConfig 是执行沙箱的运行参数（形态见 docs/24）。
type SandboxConfig struct {
	Enabled   bool
	Distro    string
	Image     string
	Network   string
	Subnet    string
	JobRoot   string
	Timeout   time.Duration
	MaxOutput int
}

// AuthConfig 是鉴权配置。
//
// Mode：
//   - auto（默认）：配了令牌就启用，没配就关闭（仅允许回环监听）；
//   - on：强制启用（没配令牌 = 启动失败）；
//   - off：强制关闭（监听非回环 = 启动失败）。
type AuthConfig struct {
	Mode         string
	TokenSpec    string
	SessionTTL   time.Duration
	CookieSecure bool
}

// Enabled 判断鉴权是否启用。
func (a AuthConfig) Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(a.Mode)) {
	case "on", "1", "true", "yes":
		return true
	case "off", "0", "false", "no":
		return false
	}
	return strings.TrimSpace(a.TokenSpec) != ""
}

// ValidateServe 是启动前的安全自检：**监听非回环时必须启用鉴权**。
//
// 这是"默认拒绝"原则在部署面的延伸：一个能暂停任务、审批危险操作、导出攻击脚本的
// 控制台，绝不能因为忘了配一个环境变量就裸奔在 0.0.0.0 上。
func (c *Config) ValidateServe() error {
	if c.Auth.Enabled() {
		if strings.TrimSpace(c.Auth.TokenSpec) == "" {
			return fmt.Errorf("QINYUAN_AUTH=on 但没有配置令牌（QINYUAN_AUTH_TOKEN / QINYUAN_AUTH_TOKENS）")
		}
		return nil
	}
	if !IsLoopbackAddr(c.Addr) {
		return fmt.Errorf("监听 %q 不是回环地址，但鉴权未启用："+
			"请配置 QINYUAN_AUTH_TOKEN=<随机长令牌>（或把监听地址改回 127.0.0.1）", c.Addr)
	}
	return nil
}

// IsLoopbackAddr 判断监听地址是否只对本机开放。
//
// 地址为空 / 端口前无 host / 0.0.0.0 / :: 都算"对外暴露"（不 loopback）。
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 没有端口（或格式怪）：按 host 判断；"localhost" 之外一律保守拒绝。
		host = strings.TrimSpace(addr)
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false // ":8787" = 所有网卡
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ConnectorConfig 是双向集成的外部端点（工单 / 资产回写）。
//
// 刻意不做成"可插拔插件目录"：先支持"任意 HTTP 端点"这一种形态，
// 客户用中间层适配自家系统；等真有第二家再做 SDK。
type ConnectorConfig struct {
	AssetsURL   string
	AssetsToken string
	TicketURL   string
	TicketToken string
}

// LLMConfig 是模型接入配置。
//
// 优先级：ENV > .env.local > 默认。API Key 只从环境/文件读，
// 不进数据库、不进日志（Summary 会打码）。
type LLMConfig struct {
	Provider    string // fake | openai
	BaseURL     string
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float64
	Timeout     time.Duration
	MaxRetries  int
}

// Summary 是给启动日志用的一行描述（不含密钥）。
func (l LLMConfig) Summary() string {
	key := "unset"
	if l.APIKey != "" {
		key = "set(" + l.KeyHint() + ")"
	}
	return fmt.Sprintf("provider=%s model=%s base_url=%s key=%s", l.Provider, l.Model, l.BaseURL, key)
}

// KeyHint 只露出末 4 位，用于确认"配的是哪把 key"。
func (l LLMConfig) KeyHint() string {
	if len(l.APIKey) <= 4 {
		return "***"
	}
	return "***" + l.APIKey[len(l.APIKey)-4:]
}

// Load 按 ENV > .env.local > 默认 的顺序装配配置。
func Load() (*Config, error) {
	fileVals, err := readDotEnv(".env.local")
	if err != nil {
		return nil, err
	}

	c := &Config{
		DataDir:             "data",
		Addr:                "127.0.0.1:8787",
		LogLevel:            "info",
		MaxToolOutput:       30000,
		DefaultMaxTurns:     12,
		DefaultTimeout:      10 * time.Minute,
		MaxUploadMB:         32,
		GoalsMaxTurns:       8,
		PlannerFullEvery:    20,
		DeltaLimit:          200,
		ContextWindowK:      200,
		SettlementTurns:     3,
		SettlementTimeout:   2 * time.Minute,
		ApprovalTimeout:     5 * time.Minute,
		ApprovalOnTimeout:   "deny",
		ApprovalLowSeconds:  60,
		ApprovalMidSeconds:  300,
		ApprovalHighSeconds: 900,
		ApprovalLowAction:   "allow",
		ApprovalMidAction:   "deny",
		ApprovalHighAction:  "deny",
		Auth: AuthConfig{
			Mode:       "auto",
			SessionTTL: 12 * time.Hour,
		},
		LLM: LLMConfig{
			Provider:    "fake",
			BaseURL:     "https://api.openai.com/v1",
			MaxTokens:   4096,
			Temperature: 0.2,
			Timeout:     120 * time.Second,
			MaxRetries:  2,
		},
	}

	dsn, origin := pick("QINYUAN_DSN", fileVals, "postgres://postgres@127.0.0.1:55432/qinyuan?sslmode=disable")
	c.DSN, c.DSNOrigin = dsn, origin

	c.DataDir, _ = pick("QINYUAN_DATA_DIR", fileVals, c.DataDir)
	c.Addr, _ = pick("QINYUAN_ADDR", fileVals, c.Addr)
	c.LogLevel, _ = pick("QINYUAN_LOG_LEVEL", fileVals, c.LogLevel)

	if v, _ := pick("QINYUAN_ALLOW_LOOPBACK", fileVals, "0"); v == "1" || strings.EqualFold(v, "true") {
		c.AllowLoopback = true
	}
	if v, _ := pick("QINYUAN_MAX_TOOL_OUTPUT", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("QINYUAN_MAX_TOOL_OUTPUT 非法：%q", v)
		}
		c.MaxToolOutput = n
	}
	if v, _ := pick("QINYUAN_MAX_TURNS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("QINYUAN_MAX_TURNS 非法：%q", v)
		}
		c.DefaultMaxTurns = n
	}
	if v, _ := pick("QINYUAN_MAX_UPLOAD_MB", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 512 {
			return nil, fmt.Errorf("QINYUAN_MAX_UPLOAD_MB 非法：%q（1..512）", v)
		}
		c.MaxUploadMB = n
	}
	if v, _ := pick("QINYUAN_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("QINYUAN_TIMEOUT_SECONDS 非法：%q", v)
		}
		c.DefaultTimeout = time.Duration(n) * time.Second
	}
	// 任务级默认墙钟（秒）：建任务时没给 timeout 就用它（0 = 保持"不限"）。
	// CTF 类任务 20 分钟不够（run #43：前 12 分钟都在读源码），默认 45 分钟。
	c.DefaultTaskTimeout = 2700
	if v, _ := pick("QINYUAN_DEFAULT_TASK_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 14400 {
			return nil, fmt.Errorf("QINYUAN_DEFAULT_TASK_TIMEOUT_SECONDS 非法：%q（0..14400）", v)
		}
		c.DefaultTaskTimeout = n
	}
	if v, _ := pick("QINYUAN_GOALS_MAX_TURNS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			return nil, fmt.Errorf("QINYUAN_GOALS_MAX_TURNS 非法：%q（1..30）", v)
		}
		c.GoalsMaxTurns = n
	}
	if v, _ := pick("QINYUAN_PLANNER_FULL_EVERY", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("QINYUAN_PLANNER_FULL_EVERY 非法：%q（正整数；0 不允许，想永远增量请设很大）", v)
		}
		c.PlannerFullEvery = n
	}
	if v, _ := pick("QINYUAN_DELTA_LIMIT", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 5000 {
			return nil, fmt.Errorf("QINYUAN_DELTA_LIMIT 非法：%q（10..5000）", v)
		}
		c.DeltaLimit = n
	}
	if v, _ := pick("QINYUAN_CONTEXT_WINDOW_K", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 2000 {
			return nil, fmt.Errorf("QINYUAN_CONTEXT_WINDOW_K 非法：%q（0=关闭压缩，1..2000）", v)
		}
		c.ContextWindowK = n
	}
	if v, _ := pick("QINYUAN_SETTLEMENT_TURNS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 20 {
			return nil, fmt.Errorf("QINYUAN_SETTLEMENT_TURNS 非法：%q（0=关闭收尾轮，1..20）", v)
		}
		c.SettlementTurns = n
	}
	if v, _ := pick("QINYUAN_SETTLEMENT_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 900 {
			return nil, fmt.Errorf("QINYUAN_SETTLEMENT_TIMEOUT_SECONDS 非法：%q（10..900）", v)
		}
		c.SettlementTimeout = time.Duration(n) * time.Second
	}
	if v, _ := pick("QINYUAN_MAX_CONCURRENT_RUNS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return nil, fmt.Errorf("QINYUAN_MAX_CONCURRENT_RUNS 非法：%q（0=不限，1..100）", v)
		}
		c.MaxConcurrentRuns = n
	}
	if v, _ := pick("QINYUAN_APPROVAL_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 3600 {
			return nil, fmt.Errorf("QINYUAN_APPROVAL_TIMEOUT_SECONDS 非法：%q（10..3600）", v)
		}
		c.ApprovalTimeout = time.Duration(n) * time.Second
	}
	if v, _ := pick("QINYUAN_APPROVAL_ON_TIMEOUT", fileVals, ""); v != "" {
		switch v {
		case "deny", "allow":
			c.ApprovalOnTimeout = v
		default:
			return nil, fmt.Errorf("QINYUAN_APPROVAL_ON_TIMEOUT 非法：%q（deny|allow）", v)
		}
	}
	if v, _ := pick("QINYUAN_APPROVAL_DISABLE", fileVals, ""); v == "1" {
		c.ApprovalDisabled = true
	}
	// 分级审批（P3-4）：秒数只能在 [5, 3600] 内，动作只认 allow/deny。
	for _, tier := range []struct {
		key string
		dst *int
		def int
	}{
		{"QINYUAN_APPROVAL_LOW_SECONDS", &c.ApprovalLowSeconds, c.ApprovalLowSeconds},
		{"QINYUAN_APPROVAL_MID_SECONDS", &c.ApprovalMidSeconds, c.ApprovalMidSeconds},
		{"QINYUAN_APPROVAL_HIGH_SECONDS", &c.ApprovalHighSeconds, c.ApprovalHighSeconds},
	} {
		if v, _ := pick(tier.key, fileVals, ""); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 5 || n > 3600 {
				return nil, fmt.Errorf("%s 非法：%q（5..3600）", tier.key, v)
			}
			*tier.dst = n
		}
	}
	for _, act := range []struct {
		key string
		dst *string
	}{
		{"QINYUAN_APPROVAL_LOW_ACTION", &c.ApprovalLowAction},
		{"QINYUAN_APPROVAL_MID_ACTION", &c.ApprovalMidAction},
		{"QINYUAN_APPROVAL_HIGH_ACTION", &c.ApprovalHighAction},
	} {
		if v, _ := pick(act.key, fileVals, ""); v != "" {
			if v != "allow" && v != "deny" {
				return nil, fmt.Errorf("%s 非法：%q（allow|deny）", act.key, v)
			}
			*act.dst = v
		}
	}

	c.LLM.Provider, _ = pick("QINYUAN_LLM_PROVIDER", fileVals, c.LLM.Provider)
	// M9：鉴权（令牌只从 ENV/文件读，不进数据库、不进日志）。
	c.Auth.Mode, _ = pick("QINYUAN_AUTH", fileVals, c.Auth.Mode)
	// 两个变量都支持：单令牌（老/简单）与多令牌带角色（token:viewer,token2:admin）。
	single, _ := pick("QINYUAN_AUTH_TOKEN", fileVals, "")
	multi, _ := pick("QINYUAN_AUTH_TOKENS", fileVals, "")
	switch {
	case single != "" && multi != "":
		c.Auth.TokenSpec = single + "," + multi
	case single != "":
		c.Auth.TokenSpec = single
	default:
		c.Auth.TokenSpec = multi
	}
	if v, _ := pick("QINYUAN_SESSION_TTL_HOURS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 168 {
			return nil, fmt.Errorf("QINYUAN_SESSION_TTL_HOURS 非法：%q（1..168）", v)
		}
		c.Auth.SessionTTL = time.Duration(n) * time.Hour
	}
	if v, _ := pick("QINYUAN_COOKIE_SECURE", fileVals, ""); v == "1" {
		c.Auth.CookieSecure = true
	}
	// P3-3：连接器端点（只在配置了 URL 时才启用；token 只从 ENV/文件读）。
	c.Connectors.AssetsURL, _ = pick("QINYUAN_CONNECTOR_ASSETS_URL", fileVals, "")
	c.Connectors.AssetsToken, _ = pick("QINYUAN_CONNECTOR_ASSETS_TOKEN", fileVals, "")
	c.Connectors.TicketURL, _ = pick("QINYUAN_CONNECTOR_TICKET_URL", fileVals, "")
	c.Connectors.TicketToken, _ = pick("QINYUAN_CONNECTOR_TICKET_TOKEN", fileVals, "")
	c.LLM.BaseURL, _ = pick("QINYUAN_LLM_BASE_URL", fileVals, c.LLM.BaseURL)
	c.LLM.APIKey, _ = pick("QINYUAN_LLM_API_KEY", fileVals, c.LLM.APIKey)
	c.LLM.Model, _ = pick("QINYUAN_LLM_MODEL", fileVals, c.LLM.Model)
	if v, _ := pick("QINYUAN_LLM_MAX_TOKENS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("QINYUAN_LLM_MAX_TOKENS 非法：%q", v)
		}
		c.LLM.MaxTokens = n
	}
	if v, _ := pick("QINYUAN_LLM_TEMPERATURE", fileVals, ""); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 2 {
			return nil, fmt.Errorf("QINYUAN_LLM_TEMPERATURE 非法：%q", v)
		}
		c.LLM.Temperature = f
	}
	if v, _ := pick("QINYUAN_LLM_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("QINYUAN_LLM_TIMEOUT_SECONDS 非法：%q", v)
		}
		c.LLM.Timeout = time.Duration(n) * time.Second
	}
	if v, _ := pick("QINYUAN_LLM_MAX_RETRIES", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 5 {
			return nil, fmt.Errorf("QINYUAN_LLM_MAX_RETRIES 非法：%q", v)
		}
		c.LLM.MaxRetries = n
	}

	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("解析数据目录失败：%w", err)
	}
	c.DataDir = abs

	// M17：执行沙箱（默认关闭；WSL2 + Docker）。
	if v, _ := pick("QINYUAN_SANDBOX", fileVals, "off"); v == "on" || v == "docker" || v == "1" || strings.EqualFold(v, "true") {
		c.Sandbox.Enabled = true
	}
	c.Sandbox.Distro, _ = pick("QINYUAN_SANDBOX_DISTRO", fileVals, "Ubuntu-24.04")
	c.Sandbox.Image, _ = pick("QINYUAN_SANDBOX_IMAGE", fileVals, "qinyuan/sandbox:latest")
	c.Sandbox.Network, _ = pick("QINYUAN_SANDBOX_NETWORK", fileVals, "qy-sandbox")
	c.Sandbox.Subnet, _ = pick("QINYUAN_SANDBOX_SUBNET", fileVals, "10.201.0.0/24")
	c.Sandbox.JobRoot, _ = pick("QINYUAN_SANDBOX_JOBROOT", fileVals, "/var/lib/qinyuan/jobs")
	if v, _ := pick("QINYUAN_SANDBOX_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 || n > 900 {
			return nil, fmt.Errorf("QINYUAN_SANDBOX_TIMEOUT_SECONDS 非法：%q（5..900）", v)
		}
		c.Sandbox.Timeout = time.Duration(n) * time.Second
	}
	if c.Sandbox.Timeout <= 0 {
		c.Sandbox.Timeout = 120 * time.Second
	}
	c.Sandbox.MaxOutput = c.MaxToolOutput
	return c, nil
}

func pick(key string, fileVals map[string]string, def string) (string, Source) {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), SourceEnv
	}
	if v, ok := fileVals[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), SourceFile
	}
	return def, SourceDefault
}

// readDotEnv 只实现 .env 的最小子集：KEY=VALUE、# 注释、可选引号。
// 不引入第三方依赖，也不支持 export/multiline。
func readDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out, sc.Err()
}

// RedactedDSN 用于日志：把密码打码。
func (c *Config) RedactedDSN() string {
	return RedactDSN(c.DSN)
}

// RedactDSN 把 DSN 里的 password 替换成 ***。
func RedactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return dsn
	}
	cred := dsn[scheme+3 : at]
	if i := strings.Index(cred, ":"); i >= 0 {
		cred = cred[:i] + ":***"
	}
	return dsn[:scheme+3] + cred + dsn[at:]
}
