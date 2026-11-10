// Package config 负责"配置从哪来、优先谁、来源是什么"。
//
// 依据《学习.md》10.5：ENV > 文件 > 默认，并且要返回来源供启动日志；
// 数据目录一律转绝对路径（相对路径在换 CWD 后会丢数据）。
package config

import (
	"bufio"
	"fmt"
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

	// M3.5：调度与上下文成本的可调项（都有安全默认值，不改也能跑）。
	GoalsMaxTurns    int // 第 0 轮目标拆解的回合上限
	PlannerFullEvery int // planner 每 K 轮给一次全量态势
	DeltaLimit       int // 单轮增量最多多少条变化（超过退全量）

	// M4：长跑可靠性。
	ContextWindowK    int // 上下文窗口（K tokens）：压缩用的估计值；0 = 关闭压缩
	SettlementTurns   int // 撞预算后的收尾轮数（0 = 关闭）
	SettlementTimeout time.Duration
	MaxConcurrentRuns int // 同时跑的任务上限（0 = 不限）

	LLM LLMConfig
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
		DataDir:           "data",
		Addr:              "127.0.0.1:8787",
		LogLevel:          "info",
		MaxToolOutput:     30000,
		DefaultMaxTurns:   12,
		DefaultTimeout:    10 * time.Minute,
		GoalsMaxTurns:     8,
		PlannerFullEvery:  20,
		DeltaLimit:        200,
		ContextWindowK:    200,
		SettlementTurns:   3,
		SettlementTimeout: 2 * time.Minute,
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
	if v, _ := pick("QINYUAN_TIMEOUT_SECONDS", fileVals, ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("QINYUAN_TIMEOUT_SECONDS 非法：%q", v)
		}
		c.DefaultTimeout = time.Duration(n) * time.Second
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

	c.LLM.Provider, _ = pick("QINYUAN_LLM_PROVIDER", fileVals, c.LLM.Provider)
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
