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
}

// Load 按 ENV > .env.local > 默认 的顺序装配配置。
func Load() (*Config, error) {
	fileVals, err := readDotEnv(".env.local")
	if err != nil {
		return nil, err
	}

	c := &Config{
		DataDir:         "data",
		Addr:            "127.0.0.1:8787",
		LogLevel:        "info",
		MaxToolOutput:   30000,
		DefaultMaxTurns: 12,
		DefaultTimeout:  10 * time.Minute,
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
