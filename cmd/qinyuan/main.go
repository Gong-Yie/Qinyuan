// 钦原（Qinyuan）· 授权 Web 安全测试 Agent。
//
// 这个二进制在 M1 提供四件事：迁移、CLI 驱动一次完整闭环、最小 REST、可复现 demo。
// 用法：qinyuan help
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"qinyuan/internal/agent"
	"qinyuan/internal/auth"
	"qinyuan/internal/config"
	"qinyuan/internal/connectors"
	"qinyuan/internal/engine"
	"qinyuan/internal/llm"
	"qinyuan/internal/model"
	"qinyuan/internal/outbox"
	"qinyuan/internal/runplan"
	"qinyuan/internal/sandbox"
	"qinyuan/internal/server"
	"qinyuan/internal/store"
)

const usage = `钦原 qinyuan —— 授权 Web 安全测试 Agent（M1）

用法：
  qinyuan help
  qinyuan version

  qinyuan migrate                      建表/升级 schema（幂等，可反复执行）
  qinyuan serve [--addr 127.0.0.1:8787] 启 HTTP API

  qinyuan run create --goal <目标> [--target <URL/IP/域名>] [--timeout 秒] [--workers N]
  qinyuan run list
  qinyuan run show --id <runID>

  qinyuan intent add --run <runID> --summary <一句话方向> [--priority 0-10] [--asset ID...]
  qinyuan intent list --run <runID> [--state open|running|done|...]

  qinyuan work --run <runID> [--once] [--turns 12] [--timeout 600]   默认起 planner + workers
  qinyuan status --run <runID>

  qinyuan demo [--keep]                 端到端演示（本地假目标 + 假模型，不联网）

环境变量（也可写在 .env.local）：
  QINYUAN_DSN               默认 postgres://postgres@127.0.0.1:55432/qinyuan?sslmode=disable
  QINYUAN_DATA_DIR          默认 ./data（会转绝对路径）
  QINYUAN_ADDR              默认 127.0.0.1:8787
  QINYUAN_ALLOW_LOOPBACK    1 = 允许访问回环地址（demo 用；默认拒绝）
  QINYUAN_MAX_TOOL_OUTPUT   工具输出上限，默认 30000 字符
  QINYUAN_MAX_TURNS         worker 每次执行回合上限，默认 12
  QINYUAN_TIMEOUT_SECONDS   worker 每次执行时长上限，默认 600

  QINYUAN_LLM_PROVIDER      fake（默认）| openai
  QINYUAN_LLM_BASE_URL      OpenAI 兼容端点，如 https://api.deepseek.com/v1
  QINYUAN_LLM_API_KEY       只从环境/文件读，不入库、不进日志
  QINYUAN_LLM_MODEL         模型名
  QINYUAN_LLM_MAX_TOKENS    默认 4096
  QINYUAN_LLM_TEMPERATURE   默认 0.2
  QINYUAN_LLM_TIMEOUT_SECONDS 默认 120
  QINYUAN_LLM_MAX_RETRIES   默认 2

  QINYUAN_AUTH              auto（默认：有令牌就启用）| on | off
  QINYUAN_AUTH_TOKEN        单令牌（默认角色 admin）
  QINYUAN_AUTH_TOKENS       多令牌带角色：tok:viewer,tok2:operator,tok3:admin
  QINYUAN_SESSION_TTL_HOURS 会话时长，默认 12（1..168）；重启即失效
  QINYUAN_COOKIE_SECURE     1 = 会话 cookie 只走 HTTPS（反代/生产开）

  ⚠ 监听非回环地址（如 0.0.0.0:8787）却没有配令牌时，serve 会拒绝启动。
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var code int
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "version":
		fmt.Println("qinyuan", server.Version)
	case "migrate":
		code = cmdMigrate(args, log)
	case "serve":
		code = cmdServe(args, log)
	case "run":
		code = cmdRun(args, log)
	case "intent":
		code = cmdIntent(args, log)
	case "work":
		code = cmdWork(args, log)
	case "status":
		code = cmdStatus(args, log)
	case "retest":
		code = cmdRetest(args, log)
	case "demo":
		code = cmdDemo(args, log)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

// ---------- 公共装配 ----------

func mustConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误：", err)
		os.Exit(1)
	}
	return cfg
}

func mustStore(ctx context.Context, cfg *config.Config) *store.Store {
	st, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败：", err)
		fmt.Fprintln(os.Stderr, "提示：先跑 scripts/devdb.ps1 start，或设置 QINYUAN_DSN")
		os.Exit(1)
	}
	if err := st.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败：", err)
		os.Exit(1)
	}
	return st
}

// buildProviders 按配置选 provider：openai（真实模型）或 fake（脚本/测试）。
func buildProviders(cfg *config.Config, log *slog.Logger) (llm.Factory, error) {
	switch strings.ToLower(cfg.LLM.Provider) {
	case "", "fake":
		log.Warn("当前使用 fake provider：不会真的调用模型", "hint", "设置 QINYUAN_LLM_PROVIDER=openai 并配 base_url/model/key")
		return llm.NewFakeFactory(llm.Step{
			Text: "（fake provider）已收到本轮输入，但没有真实模型可调用。请配置 QINYUAN_LLM_PROVIDER=openai。",
		}), nil
	case "openai":
		f, err := llm.NewOpenAIFactory(llm.OpenAIConfig{
			BaseURL:     cfg.LLM.BaseURL,
			APIKey:      cfg.LLM.APIKey,
			Model:       cfg.LLM.Model,
			MaxTokens:   cfg.LLM.MaxTokens,
			Temperature: cfg.LLM.Temperature,
			Timeout:     cfg.LLM.Timeout,
			MaxRetries:  cfg.LLM.MaxRetries,
		})
		if err != nil {
			return nil, err
		}
		log.Info("[llm] 使用 OpenAI 兼容 provider", "config", cfg.LLM.Summary())
		return f, nil
	default:
		return nil, fmt.Errorf("未知 QINYUAN_LLM_PROVIDER=%q（支持 openai / fake）", cfg.LLM.Provider)
	}
}

func newEngine(cfg *config.Config, st *store.Store, providers llm.Factory, log *slog.Logger) (*engine.Engine, error) {
	// 复测要能读到落盘的原始请求体（DB 里只有 8KB 预览）。
	agent.SetTrafficDataDir(cfg.DataDir)
	var box sandbox.Runner
	if cfg.Sandbox.Enabled {
		w := sandbox.NewWSLDocker(sandbox.Config{
			Distro: cfg.Sandbox.Distro, Image: cfg.Sandbox.Image,
			Network: cfg.Sandbox.Network, Subnet: cfg.Sandbox.Subnet,
			JobRoot: cfg.Sandbox.JobRoot, Timeout: cfg.Sandbox.Timeout,
			MaxOutput: cfg.Sandbox.MaxOutput,
		})
		checkCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := w.Check(checkCtx)
		cancel()
		if err != nil {
			// 显式开启却不可用：启动就失败，别让 exec 在运行期悄悄变成"没启用"。
			return nil, fmt.Errorf("QINYUAN_SANDBOX=on 但沙箱不可用：%w", err)
		}
		box = w
		log.Info("[sandbox] 执行沙箱已启用", "distro", cfg.Sandbox.Distro, "image", cfg.Sandbox.Image, "network", cfg.Sandbox.Network)
	}
	return engine.New(st, providers, agent.NewWorkerRegistry(), agent.NewPlannerRegistry(), engine.Config{
		DataDir:             cfg.DataDir,
		AllowLoopback:       cfg.AllowLoopback,
		MaxOutput:           cfg.MaxToolOutput,
		MaxTurns:            cfg.DefaultMaxTurns,
		MaxDuration:         cfg.DefaultTimeout,
		IdleSleep:           500 * time.Millisecond,
		Debounce:            800 * time.Millisecond,
		GoalsMaxTurns:       cfg.GoalsMaxTurns,
		PlannerFullEvery:    cfg.PlannerFullEvery,
		DeltaLimit:          cfg.DeltaLimit,
		SettlementTurns:     cfg.SettlementTurns,
		SettlementTimeout:   cfg.SettlementTimeout,
		ContextWindowTokens: cfg.ContextWindowK * 1000,
		MaxConcurrentRuns:   cfg.MaxConcurrentRuns,
		ApprovalTimeout:     cfg.ApprovalTimeout,
		ApprovalOnTimeout:   cfg.ApprovalOnTimeout,
		ApprovalDisabled:    cfg.ApprovalDisabled,
		ApprovalLowSeconds:  cfg.ApprovalLowSeconds,
		ApprovalMidSeconds:  cfg.ApprovalMidSeconds,
		ApprovalHighSeconds: cfg.ApprovalHighSeconds,
		ApprovalLowAction:   cfg.ApprovalLowAction,
		ApprovalMidAction:   cfg.ApprovalMidAction,
		ApprovalHighAction:  cfg.ApprovalHighAction,
		Sandbox:             box,
	}, log), nil
}

// buildConnectors 按配置装配外部连接器（P3-3）。
//
// 没配任何 URL 就返回 nil：未启用时行为必须与升级前逐字节一致。
func buildConnectors(cfg *config.Config, st *store.Store, log *slog.Logger) *connectors.Fanout {
	var sinks []connectors.Sink
	if cfg.Connectors.AssetsURL != "" {
		sinks = append(sinks, &connectors.Webhook{
			Label: "assets-writeback", URL: cfg.Connectors.AssetsURL, Token: cfg.Connectors.AssetsToken,
		})
	}
	if cfg.Connectors.TicketURL != "" {
		sinks = append(sinks, &connectors.Webhook{
			Label: "ticket", URL: cfg.Connectors.TicketURL, Token: cfg.Connectors.TicketToken,
		})
	}
	if len(sinks) == 0 {
		return nil
	}
	log.Info("[connector] 已启用外部连接器", "count", len(sinks))
	return &connectors.Fanout{Store: st, Sinks: sinks, Log: log}
}

// buildAuth 按配置装配鉴权层（M9）。
//
// 关：只允许回环监听（cmdServe 会先做 ValidateServe，配置错直接拒绝启动）。
// 开：令牌只从 ENV/.env.local 读，日志只打数量与角色分布，绝不打令牌本身。
func buildAuth(cfg *config.Config, log *slog.Logger) (*auth.Authenticator, error) {
	if !cfg.Auth.Enabled() {
		log.Warn("[auth] 未启用（仅在回环监听时允许）",
			"hint", "设置 QINYUAN_AUTH_TOKEN=<长随机串> 可启用；多角色用 QINYUAN_AUTH_TOKENS=tok:viewer,tok2:admin")
		return auth.Disabled(), nil
	}
	tokens, err := auth.ParseTokens(cfg.Auth.TokenSpec)
	if err != nil {
		return nil, err
	}
	byRole := map[string]int{}
	for _, r := range tokens {
		byRole[r.String()]++
	}
	log.Info("[auth] 已启用", "tokens", len(tokens), "ttl", cfg.Auth.SessionTTL.String(),
		"cookie_secure", cfg.Auth.CookieSecure, "roles", byRole)
	return auth.New(auth.Options{
		Tokens: tokens, TTL: cfg.Auth.SessionTTL, CookieSecure: cfg.Auth.CookieSecure,
		Logger: func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}), nil
}

// ---------- migrate ----------

func cmdMigrate(args []string, log *slog.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := mustConfig()
	st, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败：", err)
		return 1
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败：", err)
		return 1
	}
	fmt.Println("迁移完成（幂等）。DSN:", cfg.RedactedDSN(), "来源:", cfg.DSNOrigin)
	return 0
}

// ---------- serve ----------

func cmdServe(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "监听地址（默认取配置）")
	_ = fs.Parse(args)

	cfg := mustConfig()
	if *addr != "" {
		cfg.Addr = *addr
	}
	// M9：监听非回环却没配鉴权 = 拒绝启动（安全自检，不能靠人记得）。
	if err := cfg.ValidateServe(); err != nil {
		fmt.Fprintln(os.Stderr, "拒绝启动：", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := mustStore(ctx, cfg)
	defer st.Close()

	providers, err := buildProviders(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "模型配置错误：", err)
		return 1
	}
	eng, err := newEngine(cfg, st, providers, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	// 引擎循环自己持生命周期（不挂请求 ctx）；退出时显式停。
	defer eng.StopAll(engine.ErrShutdown)
	// 并发准入的兜底补位：队列里的任务不会自己启动，靠 ticker + 事件两个入口。
	loopCtx, stopLoop := context.WithCancel(context.Background())
	defer stopLoop()
	go eng.ReconcileLoop(loopCtx)
	fanout := buildConnectors(cfg, st, log)
	// P2-3：outbox 投递器。finding 入库时已经在**同一个事务**里入队，
	// 这里注册处理器把事件真正送出去（P3-3 的连接器会挂在同一个 topic 上做扇出）。
	disp := outbox.New(st, map[string]outbox.Handler{
		store.TopicFindingRecorded: func(ctx context.Context, ev store.OutboxEvent) error {
			var p struct {
				FindingID int64  `json:"finding_id"`
				Name      string `json:"name"`
				Severity  string `json:"severity"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			if _, err := st.AppendActivity(ctx, &model.Activity{
				RunID: ev.RunID, Worker: "outbox", Kind: model.ActSystem,
				Summary: fmt.Sprintf("事件已投递：%s #%d（%s，%s）", ev.Topic, p.FindingID, p.Severity, clip(p.Name, 40)),
				Metadata: map[string]any{
					"outbox_id": ev.ID, "topic": ev.Topic, "attempts": ev.Attempts,
				},
			}); err != nil {
				return err
			}
			// P3-3：扇出到外部系统（幂等由 connector_deliveries 台账保证）。
			if fanout != nil {
				return fanout.Handle(ctx, ev)
			}
			return nil
		},
	}, log)
	if st != nil && disp.Handlers != nil {
		go disp.Run(loopCtx)
	}
	// 启动恢复：崩溃残留的 running 打回 open，并给非终态任务重建循环。
	if _, err := eng.Restore(context.Background()); err != nil {
		log.Error("[engine] 启动恢复失败", "err", err)
	}
	srv := server.New(st, eng, log)
	srv.Connectors = fanout
	srv.MaxUploadMB = cfg.MaxUploadMB
	srv.DefaultTaskTimeout = cfg.DefaultTaskTimeout
	authn, err := buildAuth(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "鉴权配置错误：", err)
		return 1
	}
	srv.Auth = authn

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("HTTP 监听", "addr", cfg.Addr, "version", server.Version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务退出", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Info("收到信号，优雅退出")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = httpSrv.Shutdown(shutdownCtx)
	return 0
}

// ---------- run ----------

func cmdRun(args []string, log *slog.Logger) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法：qinyuan run create|list|show ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch sub {
	case "create":
		fs := flag.NewFlagSet("run create", flag.ExitOnError)
		goal := fs.String("goal", "", "目标（自然语言，必填）")
		desc := fs.String("description", "", "背景/范围说明")
		name := fs.String("name", "", "任务名")
		target := fs.String("target", "", "目标 URL / IP / 域名 / 网段")
		timeout := fs.Int("timeout", 0, "任务级超时秒数（0=用默认；默认见 QINYUAN_DEFAULT_TASK_TIMEOUT_SECONDS）")
		workers := fs.Int("workers", 2, "worker 数")
		_ = fs.Parse(rest)
		cfg := mustConfig()
		st := mustStore(ctx, cfg)
		defer st.Close()

		if *timeout <= 0 {
			*timeout = cfg.DefaultTaskTimeout
		}
		in, err := runplan.Build(runplan.Request{
			Goal: *goal, Description: *desc, Name: *name, Target: *target,
			TimeoutSeconds: *timeout, WorkerCount: *workers,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "参数错误：", err)
			return 2
		}
		run, err := st.CreateRun(ctx, in)
		if err != nil {
			fmt.Fprintln(os.Stderr, "建任务失败：", err)
			return 1
		}
		fmt.Printf("run #%d 已创建：%s\n", run.ID, run.Goal)
		if *target != "" {
			fmt.Printf("授权范围：%s（最小范围原则）\n", *target)
		}
		return 0

	case "list":
		cfg := mustConfig()
		st := mustStore(ctx, cfg)
		defer st.Close()
		runs, err := st.ListRuns(ctx, 50)
		if err != nil {
			fmt.Fprintln(os.Stderr, "查询失败：", err)
			return 1
		}
		if len(runs) == 0 {
			fmt.Println("还没有任务。qinyuan run create --goal ...")
			return 0
		}
		fmt.Printf("%-5s %-10s %-6s %s\n", "ID", "状态", "轮次", "目标")
		for _, r := range runs {
			active, _ := st.HasActiveIntent(ctx, r.ID)
			fmt.Printf("%-5d %-10s %-6d %s\n", r.ID, model.DeriveStatus(r.Task, active, false), r.RoundNo, clip(r.Goal, 60))
		}
		return 0

	case "show":
		fs := flag.NewFlagSet("run show", flag.ExitOnError)
		id := fs.Int64("id", 0, "run id")
		_ = fs.Parse(rest)
		if *id <= 0 {
			fmt.Fprintln(os.Stderr, "--id 必填")
			return 2
		}
		cfg := mustConfig()
		st := mustStore(ctx, cfg)
		defer st.Close()
		return printStatus(ctx, st, *id)

	default:
		fmt.Fprintf(os.Stderr, "未知子命令 run %s\n", sub)
		return 2
	}
}

// ---------- intent ----------

func cmdIntent(args []string, log *slog.Logger) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法：qinyuan intent add|list ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch sub {
	case "add":
		fs := flag.NewFlagSet("intent add", flag.ExitOnError)
		runID := fs.Int64("run", 0, "run id")
		summary := fs.String("summary", "", "一句话方向（必填）")
		priority := fs.Int("priority", 5, "优先级 0-10")
		_ = fs.Parse(rest)
		if *runID <= 0 || *summary == "" {
			fmt.Fprintln(os.Stderr, "--run 与 --summary 必填")
			return 2
		}
		cfg := mustConfig()
		st := mustStore(ctx, cfg)
		defer st.Close()
		id, err := st.AddIntent(ctx, store.AddIntentInput{
			RunID: *runID, Summary: *summary, Priority: *priority, Origin: "manual",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "建意图失败：", err)
			return 1
		}
		fmt.Printf("intent #%d 已创建（state=open）\n", id)
		return 0

	case "list":
		fs := flag.NewFlagSet("intent list", flag.ExitOnError)
		runID := fs.Int64("run", 0, "run id")
		state := fs.String("state", "", "只看某状态")
		_ = fs.Parse(rest)
		if *runID <= 0 {
			fmt.Fprintln(os.Stderr, "--run 必填")
			return 2
		}
		cfg := mustConfig()
		st := mustStore(ctx, cfg)
		defer st.Close()
		nodes, err := st.NodesByKind(ctx, *runID, model.KindIntent, 200)
		if err != nil {
			fmt.Fprintln(os.Stderr, "查询失败：", err)
			return 1
		}
		fmt.Printf("%-5s %-10s %-4s %s\n", "ID", "状态", "P", "摘要")
		for _, n := range nodes {
			if *state != "" && n.State != *state {
				continue
			}
			fmt.Printf("%-5d %-10s %-4d %s\n", n.ID, n.State, n.Priority, clip(n.Summary(), 70))
		}
		return 0

	default:
		fmt.Fprintf(os.Stderr, "未知子命令 intent %s\n", sub)
		return 2
	}
}

// ---------- work ----------

func cmdWork(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("work", flag.ExitOnError)
	runID := fs.Int64("run", 0, "run id")
	once := fs.Bool("once", false, "只领一条就退出（不启动 planner）")
	workers := fs.Int("workers", 0, "保留参数；M2 起 worker 数以上建任务时的 worker_count / 任务行为准")
	turns := fs.Int("turns", 0, "覆盖每次执行回合上限")
	timeout := fs.Int("timeout", 0, "覆盖每次执行时长上限（秒）")
	_ = fs.Parse(args)
	if *runID <= 0 {
		fmt.Fprintln(os.Stderr, "--run 必填")
		return 2
	}
	_ = workers
	cfg := mustConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := mustStore(ctx, cfg)
	defer st.Close()

	// M2 起按配置选 provider：配了 openai 就是真模型，否则 fake（只验证链路）。
	providers, err := buildProviders(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "模型配置错误：", err)
		return 1
	}
	eng, err := newEngine(cfg, st, providers, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	if *turns > 0 {
		eng.Cfg.MaxTurns = *turns
	}
	if *timeout > 0 {
		eng.Cfg.MaxDuration = time.Duration(*timeout) * time.Second
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Info("收到信号，停止领取")
		cancel()
	}()

	if *once {
		claimed, err := eng.Work(ctx, *runID, "work#1")
		if err != nil {
			fmt.Fprintln(os.Stderr, "执行失败：", err)
			return 1
		}
		if !claimed {
			fmt.Println("没有可领取的意图（frontier 为空或任务已暂停/终态）")
		}
		return printStatus(ctx, st, *runID)
	}

	// 完整引擎：planner + workers 一起跑，直到任务到终态或用户中断。
	if err := eng.Run(ctx, *runID); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	for {
		run, err := st.Run(ctx, *runID)
		if err != nil {
			break
		}
		if run.Task != nil && model.IsTerminalTaskState(run.Task.Status) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	eng.StopTask(*runID, engine.ErrShutdown)
	return printStatus(context.WithoutCancel(ctx), st, *runID)
}

// ---------- status ----------

// ---------- retest ----------

// cmdRetest 复测 finding：重放它绑定的交换，按断言给 verified/failed/unverifiable。
func cmdRetest(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("retest", flag.ExitOnError)
	runID := fs.Int64("run", 0, "run id")
	findingID := fs.Int64("finding", 0, "只复测这一条 finding（0 = 全部）")
	_ = fs.Parse(args)
	if *runID <= 0 {
		fmt.Fprintln(os.Stderr, "--run 必填")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := mustConfig()
	agent.SetTrafficDataDir(cfg.DataDir)
	st := mustStore(ctx, cfg)
	defer st.Close()

	var ids []int64
	if *findingID > 0 {
		ids = []int64{*findingID}
	} else {
		nodes, err := st.NodesByKind(ctx, *runID, model.KindFinding, 100)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读发现失败：", err)
			return 1
		}
		for i := len(nodes) - 1; i >= 0; i-- {
			ids = append(ids, nodes[i].ID)
		}
	}
	if len(ids) == 0 {
		fmt.Println("本任务没有 finding 可复测。")
		return 0
	}

	fmt.Printf("%-10s %-14s %s\n", "FINDING", "结果", "说明")
	verified, failed, unverifiable := 0, 0, 0
	for _, fid := range ids {
		rows, err := agent.RetestFinding(ctx, st, *runID, fid, cfg.AllowLoopback)
		if err != nil {
			fmt.Fprintf(os.Stderr, "复测 #%d 失败：%v\n", fid, err)
			continue
		}
		for _, rt := range rows {
			switch rt.Status {
			case store.RetestVerified:
				verified++
			case store.RetestFailed:
				failed++
			default:
				unverifiable++
			}
			fmt.Printf("%-10d %-14s %s\n", fid, rt.Status, clip(rt.Observed, 72))
		}
	}
	fmt.Printf("\n合计：verified=%d failed=%d unverifiable=%d\n", verified, failed, unverifiable)
	return 0
}

func cmdStatus(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	runID := fs.Int64("run", 0, "run id")
	_ = fs.Parse(args)
	if *runID <= 0 {
		fmt.Fprintln(os.Stderr, "--run 必填")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := mustConfig()
	st := mustStore(ctx, cfg)
	defer st.Close()
	return printStatus(ctx, st, *runID)
}

func printStatus(ctx context.Context, st *store.Store, runID int64) int {
	run, err := st.Run(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "run #%d 不存在\n", runID)
			return 1
		}
		fmt.Fprintln(os.Stderr, "查询失败：", err)
		return 1
	}
	active, _ := st.HasActiveIntent(ctx, runID)
	fmt.Printf("== run #%d ==\n目标：%s\n状态：%s", run.ID, run.Goal, model.DeriveStatus(run.Task, active, false))
	if run.Task != nil {
		fmt.Printf("（task=%s paused=%v queued=%v workers=%d）", run.Task.Status, run.Task.Paused, run.Task.Queued, run.Task.WorkerCount)
	}
	fmt.Printf("\n轮次：%d  version：%d\n\n", run.RoundNo, run.Version)

	fmt.Println("-- 意图 --")
	intents, _ := st.NodesByKind(ctx, runID, model.KindIntent, 50)
	if len(intents) == 0 {
		fmt.Println("（无）")
	}
	for _, n := range intents {
		owner := ""
		if n.Owner != "" {
			owner = " owner=" + n.Owner
		}
		fmt.Printf("#%-4d %-10s P%d%s  %s\n", n.ID, n.State, n.Priority, owner, clip(n.Summary(), 72))
	}

	fmt.Println("\n-- 事实 --")
	facts, _ := st.NodesByKind(ctx, runID, model.KindFact, 20)
	if len(facts) == 0 {
		fmt.Println("（无）")
	}
	for _, n := range facts {
		fmt.Printf("#%-4d %-10s %-10s %s\n", n.ID, n.State, n.PayloadField("confidence"), clip(n.Summary(), 66))
	}

	fmt.Println("\n-- 漏洞 --")
	findings, _ := st.NodesByKind(ctx, runID, model.KindFinding, 20)
	if len(findings) == 0 {
		fmt.Println("（无）")
	}
	for _, n := range findings {
		fmt.Printf("#%-4d %-10s %-8s %s\n", n.ID, n.State, n.PayloadField("severity"), clip(n.Summary(), 60))
	}

	fmt.Println("\n-- 过程（最近 20 条）--")
	acts, _ := st.ActivityTail(ctx, runID, 20)
	for _, a := range acts {
		tool := ""
		if a.Tool != "" {
			tool = "[" + a.Tool + "]"
		}
		flagStr := ""
		if a.IsError {
			flagStr = " !ERR"
		}
		nodeStr := "-"
		if a.NodeID != nil {
			nodeStr = fmt.Sprintf("#%d", *a.NodeID)
		}
		fmt.Printf("%-6d %-12s %-12s %-3s%s%s %s\n", a.ID, a.Kind, nodeStr, flagStr, tool,
			"", clip(a.Summary, 70))
	}
	if len(acts) == 0 {
		fmt.Println("（无）")
	}

	usage, _ := st.TokenTotals(ctx, runID)
	fmt.Printf("\n-- token（只认 result 行）--\ninput=%d output=%d cache_read=%d cache_write=%d\n",
		usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)
	if rows, err := st.UsageByNode(ctx, runID); err == nil && len(rows) > 0 {
		fmt.Println("\n-- 每节点用量（按有效输入降序；node 0 = planner/goals 这类任务级调用）--")
		for _, u := range rows {
			who := "任务级"
			if u.NodeID > 0 {
				who = fmt.Sprintf("#%d %s", u.NodeID, clip(u.Summary, 26))
			}
			fmt.Printf("%-36s calls=%-3d in=%-7d cache=%-7d eff=%-7d out=%d\n",
				who, u.Calls, u.Input, u.CacheRead, u.EffectiveIn, u.Output)
		}
	}
	return 0
}

// ---------- demo ----------

// cmdDemo 端到端演示：本地起一个假目标，用假模型驱动一次真实闭环。
//
// 它证明的是"架构跑通"：范围闸门 → HTTP 工具 → 实体/事实写回 → 意图落 done →
// activity 配对 → token 记账。它不证明模型能力（M2 才接真模型）。
func cmdDemo(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	keep := fs.Bool("keep", false, "结束后保留假目标服务（默认关闭）")
	_ = fs.Parse(args)

	cfg := mustConfig()
	// demo 必须访问 127.0.0.1 上的假目标：显式打开回环访问（默认是拒绝的）。
	cfg.AllowLoopback = true

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	st := mustStore(ctx, cfg)
	defer st.Close()

	// 1) 假目标：一个带 Server 头与登录表单的页面 + 一个 403 的 /admin。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "起假目标失败：", err)
		return 1
	}
	targetURL := "http://" + ln.Addr().String() + "/"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "qinyuan-demo/1.0")
		w.Header().Set("X-Powered-By", "demo-stack")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Qinyuan Demo Login</title></head>
<body><form method="post" action="/login"><input name="username"><input name="password" type="password"></form></body></html>`)
	})
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "qinyuan-demo/1.0")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "forbidden")
	})
	targetSrv := &http.Server{Handler: mux}
	go func() { _ = targetSrv.Serve(ln) }()
	defer func() {
		if *keep {
			fmt.Printf("假目标仍在运行：%s（Ctrl+C 结束）\n", targetURL)
			return
		}
		_ = targetSrv.Close()
	}()
	fmt.Println("假目标：", targetURL)

	// 2) 建任务（目标 URL → 自动生成最小范围 + 种子实体）。
	in, err := runplan.Build(runplan.Request{
		Goal:        "确认演示站点的服务指纹与登录入口，输出一条可复核的事实",
		Name:        "demo",
		Target:      targetURL,
		WorkerCount: 1,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "建任务参数错误：", err)
		return 1
	}
	run, err := st.CreateRun(ctx, in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "建任务失败：", err)
		return 1
	}
	fmt.Printf("run #%d 已创建，范围已登记\n", run.ID)

	ents, err := st.ListEntitiesForRun(ctx, run.ID, 10)
	if err != nil || len(ents) == 0 {
		fmt.Fprintln(os.Stderr, "种子实体缺失：", err)
		return 1
	}
	assetID := ents[0].ID

	intentID, err := st.AddIntent(ctx, store.AddIntentInput{
		RunID:    run.ID,
		Summary:  "请求首页，确认服务指纹与登录表单入口（演示意图）",
		AssetIDs: []int64{assetID},
		Priority: 5,
		Origin:   "seed",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "建意图失败：", err)
		return 1
	}
	fmt.Printf("intent #%d 已创建\n", intentID)

	// 3) 假模型脚本：真实调用 http_request → 登记实体 → 写事实 → 总结。
	fake := llm.NewFakeFactory(
		llm.Step{ToolCalls: []llm.ToolCall{
			llm.NewToolCall("call_1", "http_request", map[string]any{"method": "GET", "url": targetURL}),
		}, Usage: llm.Usage{Input: 120, Output: 30}},
		llm.Step{ToolCalls: []llm.ToolCall{
			llm.NewToolCall("call_2", "insert_assets", map[string]any{"assets": []map[string]any{{
				"type": "endpoint", "url": targetURL, "method": "GET", "status_code": 200,
				"page_title": "Qinyuan Demo Login", "technologies": []string{"demo-stack"},
			}}}),
		}, Usage: llm.Usage{Input: 200, Output: 40}},
		llm.Step{ToolCalls: []llm.ToolCall{
			llm.NewToolCall("call_3", "record_fact", map[string]any{"facts": []map[string]any{{
				"summary":    "演示站点首页可访问，存在登录表单，服务头为 qinyuan-demo/1.0",
				"detail":     "GET / → 200；Server: qinyuan-demo/1.0；页面含 <form action=/login> 的账号/密码输入框",
				"evidence":   "GET " + targetURL + " → 200 Server: qinyuan-demo/1.0，title=Qinyuan Demo Login",
				"confidence": "observed",
				"asset_ids":  []int64{assetID},
			}}}),
		}, Usage: llm.Usage{Input: 300, Output: 90}},
		llm.Step{Text: "已完成：确认首页可访问、记录服务指纹与登录入口，并写入一条 observed 事实。",
			Usage: llm.Usage{Input: 80, Output: 40}},
	)

	eng, err := newEngine(cfg, st, fake, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	claimed, err := eng.Work(ctx, run.ID, "demo-worker")
	if err != nil {
		fmt.Fprintln(os.Stderr, "执行失败：", err)
		return 1
	}
	if !claimed {
		fmt.Fprintln(os.Stderr, "没有领到意图（frontier 为空）")
		return 1
	}

	fmt.Println()
	code := printStatus(ctx, st, run.ID)

	// 4) 断言闭环真的成立（demo 不只打印，还要自检）。
	intent, err := st.Intent(ctx, run.ID, intentID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读回意图失败：", err)
		return 1
	}
	facts, _ := st.NodesByKind(ctx, run.ID, model.KindFact, 10)
	acts, _ := st.ActivityTail(ctx, run.ID, 200)
	paired := map[string]bool{}
	for _, a := range acts {
		if a.Kind == model.ActToolResult && a.ToolUseID != "" {
			paired[a.ToolUseID] = true
		}
	}
	ok := true
	if intent.State != model.StateDone {
		fmt.Printf("✗ 意图终态应为 done，实际 %s\n", intent.State)
		ok = false
	}
	hasFact := false
	for _, f := range facts {
		if f.State == model.StateConfirmed {
			hasFact = true
			break
		}
	}
	if !hasFact {
		fmt.Println("✗ 没有写入 confirmed 事实")
		ok = false
	}
	for _, id := range []string{"call_1", "call_2", "call_3"} {
		if !paired[id] {
			fmt.Printf("✗ tool_use %s 没有配对的 tool_result\n", id)
			ok = false
		}
	}
	if !ok {
		return 1
	}
	fmt.Println("\n✓ demo 通过：范围闸门 → HTTP 探测 → 实体/事实写回 → 意图 done → 配对完整")
	return code
}

func clip(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
