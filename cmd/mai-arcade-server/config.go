package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/access"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// 默认值。
const (
	defaultListen = "127.0.0.1:8787" // 只绑本机：本服务持有账号凭证与查分器写权限。

	defaultRateBurst    = 6
	defaultRateInterval = 2 * time.Minute
	defaultConcurrency  = 2
	defaultOpTimeout    = 15 * time.Minute
	defaultWSIdle       = 5 * time.Minute
)

// 令牌来源，用于启动摘要里说明令牌从哪来。
const (
	tokenSourceEnv  = "环境变量"
	tokenSourceFile = "文件"
	tokenSourceArgv = "命令行参数"
)

// config 是服务的全部配置。
type config struct {
	listen     string
	allowWrite bool
	disableWS  bool

	token       string
	tokenSource string

	rateBurst    int
	rateInterval time.Duration
	concurrency  int
	maxBody      int64

	operationTimeout time.Duration
	wsIdleTimeout    time.Duration

	logLevel string

	// 以下透传给编排层。
	version        string
	proxy          string
	requestTimeout time.Duration
	sessionTimeout time.Duration
	titleBaseURL   string
	aimeURL        string
	chartBaseURL   string
}

// parseConfig 解析并校验命令行参数。
func parseConfig(args []string, stderr io.Writer) (config, error) {
	var (
		cfg      config
		token    string
		tokenEnv string
		tokenF   string
		showHelp bool
	)

	fs := flag.NewFlagSet("mai-arcade-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	fs.StringVar(&cfg.listen, "listen", defaultListen, "监听地址")
	fs.BoolVar(&cfg.allowWrite, "allow-write", false, "允许会改动外部数据的操作")
	fs.BoolVar(&cfg.disableWS, "no-ws", false, "不注册 WebSocket 端点")
	fs.StringVar(&token, "token", "", "服务令牌（会出现在 ps 里，仅用于本机调试）")
	fs.StringVar(&tokenEnv, "token-env", "", "从该环境变量读取服务令牌")
	fs.StringVar(&tokenF, "token-file", "", "从该文件读取服务令牌")
	fs.IntVar(&cfg.rateBurst, "rate-burst", defaultRateBurst, "限频窗口内允许的请求数，0 表示不限")
	fs.DurationVar(&cfg.rateInterval, "rate-interval", defaultRateInterval, "限频窗口")
	fs.IntVar(&cfg.concurrency, "concurrency", defaultConcurrency, "同时在跑的操作数上限")
	fs.Int64Var(&cfg.maxBody, "max-body", access.DefaultMaxBody, "请求体上限（字节）")
	fs.DurationVar(&cfg.operationTimeout, "op-timeout", defaultOpTimeout, "单次操作超时上限")
	fs.DurationVar(&cfg.wsIdleTimeout, "ws-idle-timeout", defaultWSIdle, "WebSocket 空闲回收时间")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "日志级别 debug/info/warn/error")
	fs.StringVar(&cfg.version, "version", "", "协议版本（默认 "+protocol.DefaultVersion+"）")
	fs.StringVar(&cfg.proxy, "proxy", "", "HTTP 代理地址")
	fs.DurationVar(&cfg.requestTimeout, "timeout", service.DefaultTimeout, "单次请求超时")
	fs.DurationVar(&cfg.sessionTimeout, "session-timeout", service.DefaultSessionTimeout, "单次会话时长上限")
	fs.StringVar(&cfg.titleBaseURL, "title-base-url", "", "标题服务器根地址（调试用）")
	fs.StringVar(&cfg.aimeURL, "aime-url", "", "AimeDB 接口地址（调试用）")
	fs.StringVar(&cfg.chartBaseURL, "chart-base-url", "", "查分器 API 根地址（调试用）")
	fs.BoolVar(&showHelp, "help", false, "显示帮助")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stderr)
		}
		return config{}, err
	}
	if showHelp {
		usage(stderr)
		return config{}, flag.ErrHelp
	}
	if rest := fs.Args(); len(rest) > 0 {
		return config{}, fmt.Errorf("多余的参数 %v", rest)
	}

	tokenValue, source, err := resolveToken(token, tokenEnv, tokenF, stderr)
	if err != nil {
		return config{}, err
	}
	cfg.token, cfg.tokenSource = tokenValue, source

	if _, err := access.NewAuthenticator(cfg.token); err != nil {
		return config{}, err
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}

	// 归一化版本号：让启动日志与自描述显示真正生效的版本，而不是空串。
	version, err := protocol.LookupVersion(cfg.version)
	if err != nil {
		return config{}, err
	}
	cfg.version = version.Encoding
	return cfg, nil
}

// validate 检查取值边界；越界的配置应当启动即失败，而不是运行时才暴露。
func (c config) validate() error {
	if c.rateBurst < 0 {
		return fmt.Errorf("--rate-burst 不能为负")
	}
	if c.rateBurst > 0 && c.rateInterval <= 0 {
		return fmt.Errorf("--rate-interval 必须为正")
	}
	if c.concurrency < 1 {
		return fmt.Errorf("--concurrency 至少为 1")
	}
	if c.maxBody <= 0 {
		return fmt.Errorf("--max-body 必须为正")
	}
	if c.operationTimeout <= 0 {
		return fmt.Errorf("--op-timeout 必须为正")
	}
	// 操作超时必须在会话超时之上留出余量，否则会话收尾（登出）会被掐断。
	if c.operationTimeout <= c.sessionTimeout {
		return fmt.Errorf("--op-timeout (%s) 必须大于 --session-timeout (%s)，否则机台会话来不及登出",
			c.operationTimeout, c.sessionTimeout)
	}
	if c.wsIdleTimeout <= 0 {
		return fmt.Errorf("--ws-idle-timeout 必须为正")
	}
	if _, err := protocol.LookupVersion(c.version); err != nil {
		return err
	}
	if _, err := c.logLevelValue(); err != nil {
		return err
	}
	return nil
}

// logLevelValue 把配置里的级别名解析成 slog 级别。
//
// validate 与 newLogger 共用它：两处各写一个 switch 迟早会判定不一致。
func (c config) logLevelValue() (slog.Level, error) {
	switch strings.ToLower(c.logLevel) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("未知日志级别 %q", c.logLevel)
	}
}

// newLogger 按配置构造结构化日志器。
func (c config) newLogger(out io.Writer) (*slog.Logger, error) {
	level, err := c.logLevelValue()
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})), nil
}

// resolveToken 从三种来源之一取令牌；同时给出两个来源时直接报错，避免猜错。
func resolveToken(fromFlag, fromEnv, fromFile string, stderr io.Writer) (string, string, error) {
	given := 0
	for _, value := range []string{fromFlag, fromEnv, fromFile} {
		if value != "" {
			given++
		}
	}
	switch {
	case given == 0:
		return "", "", fmt.Errorf("必须指定令牌来源：--token-env（推荐）、--token-file 或 --token；" +
			"本服务持有账号凭证与查分器写权限，不能无鉴权运行")
	case given > 1:
		return "", "", fmt.Errorf("--token、--token-env、--token-file 只能给一个")
	}

	switch {
	case fromEnv != "":
		value := strings.TrimSpace(os.Getenv(fromEnv))
		if value == "" {
			return "", "", fmt.Errorf("环境变量 %s 为空或未设置", fromEnv)
		}
		return value, tokenSourceEnv, nil

	case fromFile != "":
		raw, err := os.ReadFile(fromFile)
		if err != nil {
			return "", "", fmt.Errorf("读取令牌文件失败: %w", err)
		}
		warnIfWorldReadable(fromFile, stderr)
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", "", fmt.Errorf("令牌文件 %s 为空", fromFile)
		}
		return value, tokenSourceFile, nil

	default:
		return strings.TrimSpace(fromFlag), tokenSourceArgv, nil
	}
}

// warnIfWorldReadable 在令牌文件对同组或他人可读时给出警告。
func warnIfWorldReadable(path string, stderr io.Writer) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(stderr, "⚠ 令牌文件 %s 的权限是 %o，建议收紧到 600\n", path, info.Mode().Perm())
	}
}
