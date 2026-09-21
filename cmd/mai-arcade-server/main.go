// Command mai-arcade-server 把机台协议能力暴露成本地服务：HTTP 与 WebSocket 两种接入方式，
// 共用同一组操作（见 ops 包）与同一套鉴权、限频、限额。
//
// 本文件只做参数解析、依赖装配与进程生命周期管理，不含业务逻辑。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/access"
	"github.com/xzadudu179/maimai-arcade/internal/ops"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	"github.com/xzadudu179/maimai-arcade/internal/server"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// 退出码沿用 CLI 的约定：0 成功 / 1 业务失败 / 2 网络或阻断 / 3 参数错。
const (
	exitOK       = 0
	exitBusiness = 1
	exitNetwork  = 2
	exitUsage    = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是可测试的入口。
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "启动失败: %v\n", err)
		return exitUsage
	}

	logger, err := cfg.newLogger(stderr)
	if err != nil {
		fmt.Fprintf(stderr, "启动失败: %v\n", err)
		return exitUsage
	}

	svc, err := service.New(service.Options{
		Version:        cfg.version,
		ProxyURL:       cfg.proxy,
		Timeout:        cfg.requestTimeout,
		SessionTimeout: cfg.sessionTimeout,
		TitleBaseURL:   cfg.titleBaseURL,
		AimeURL:        cfg.aimeURL,
		ChartBaseURL:   cfg.chartBaseURL,
		Logger:         logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "启动失败: %v\n", err)
		return exitUsage
	}

	handler, err := buildHandler(cfg, svc, logger)
	if err != nil {
		fmt.Fprintf(stderr, "启动失败: %v\n", err)
		return exitUsage
	}

	return serve(cfg, handler, logger, stderr)
}

// buildHandler 装配接入层。
func buildHandler(cfg config, svc *service.Service, logger *slog.Logger) (http.Handler, error) {
	auth, err := access.NewAuthenticator(cfg.token)
	if err != nil {
		return nil, err
	}

	opts := server.Options{
		Deps: ops.Deps{
			Service:    svc,
			AllowWrite: cfg.allowWrite,
			Logger:     logger,
		},
		Auth:             auth,
		Limiter:          access.NewRateLimiter(cfg.rateBurst, cfg.rateInterval),
		Sem:              access.NewSemaphore(cfg.concurrency),
		MaxBody:          cfg.maxBody,
		OperationTimeout: cfg.operationTimeout,
		WSIdleTimeout:    cfg.wsIdleTimeout,
		Logger:           logger,
	}
	if cfg.disableWS {
		return server.NewHTTPHandlerWithoutWS(opts)
	}
	return server.NewHTTPHandler(opts)
}

// serve 启动并阻塞，直到收到信号或被关闭。
func serve(cfg config, handler http.Handler, logger *slog.Logger, stderr io.Writer) int {
	httpServer := &http.Server{
		Addr:    cfg.listen,
		Handler: handler,
		// 读头部超时用来挡住慢速恶意连接；请求体很小，整体读超时不必放长。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// 写超时必须覆盖最长的机台会话，否则会在登出收尾前掐断连接。
		WriteTimeout:   cfg.operationTimeout + 30*time.Second,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 16 << 10,
	}

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		fmt.Fprintf(stderr, "监听 %s 失败: %v\n", cfg.listen, err)
		return exitNetwork
	}

	printBanner(cfg, listener, logger, stderr)

	// 信号到达时优雅关闭：等待正在处理的请求收尾，避免把机台会话丢在半路。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		fmt.Fprintf(stderr, "服务异常退出: %v\n", err)
		return exitBusiness
	case <-ctx.Done():
		logger.Info("收到退出信号，等待在途请求收尾")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.operationTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("优雅关闭超时", "原因", err.Error())
			return exitBusiness
		}
		logger.Info("已关闭")
		return exitOK
	}
}

// printBanner 打印启动摘要。
//
// 它刻意不打令牌，只说明来源；并把「令牌出现在 argv 里」这类部署隐患直接点出来。
func printBanner(cfg config, listener net.Listener, logger *slog.Logger, stderr io.Writer) {
	write := "关闭（只读）"
	if cfg.allowWrite {
		write = "开启"
	}

	logger.Info("服务已启动",
		"监听", listener.Addr().String(),
		"写操作", write,
		"协议版本", cfg.version,
		"限频", fmt.Sprintf("每 %s 最多 %d 次", cfg.rateInterval, cfg.rateBurst),
		"并发上限", cfg.concurrency,
		"WebSocket", !cfg.disableWS,
		"令牌来源", cfg.tokenSource)

	if !isLoopback(cfg.listen) {
		fmt.Fprintln(stderr, "⚠ 正在监听非回环地址：本服务持有读取账号成绩并写入查分器的能力，"+
			"请确认前面有 TLS 与鉴权代理，并确保 --token 来自 --token-env 或 --token-file")
	}
	if cfg.tokenSource == tokenSourceArgv {
		fmt.Fprintln(stderr, "⚠ 令牌来自命令行参数，会被 ps 看到；生产环境请改用 --token-env 或 --token-file")
	}
	if cfg.allowWrite {
		fmt.Fprintln(stderr, "⚠ 已开启写操作：调用方可以改动查分器上的成绩数据")
	}
}

// isLoopback 报告监听地址是否只绑定本机。
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// usage 打印帮助。
func usage(w io.Writer) {
	fmt.Fprintf(w, `mai-arcade-server —— 把机台协议能力暴露成本地服务

用法:
  mai-arcade-server [参数]

接入方式:
  HTTP       POST /v1/op/{操作名}      请求体为 JSON 参数
  HTTP       GET  /v1/ops              查看可用操作与全部错误码
  HTTP       GET  /healthz             存活探针（不鉴权）
  WebSocket  GET  /v1/ws               同一组操作，消息为 JSON 文本帧

鉴权:
  所有 /v1/* 端点都需要 Bearer 令牌：
    Authorization: Bearer <token>
    X-Auth-Token: <token>                       （等价写法）
    Sec-WebSocket-Protocol: mai-arcade.v1.bearer.<token>   （浏览器 WebSocket 用）

令牌来源（三选一，推荐前两种，避免 ps 泄露）:
  --token-env NAME     从环境变量 NAME 读取
  --token-file PATH    从文件读取（首尾空白会被去掉）
  --token VALUE        直接给出（会出现在 ps 里，仅用于本机调试）

安全默认值:
  监听 127.0.0.1，写操作关闭。要同步成绩必须显式 --allow-write。

示例:
  MAI_TOKEN=$(head -c 32 /dev/urandom | base64) 
  export MAI_FISH_TOKEN=...
  mai-arcade-server --token-env MAI_TOKEN --allow-write
  curl -s localhost:8787/v1/op/probe -H "Authorization: Bearer $MAI_TOKEN"

常用参数:
  --listen string          监听地址（默认 127.0.0.1:8787）
  --allow-write            允许会改动外部数据的操作（默认关闭）
  --rate-burst int         限频窗口内允许的请求数（默认 6，0 表示不限）
  --rate-interval duration 限频窗口（默认 2m）
  --concurrency int        同时在跑的操作数上限（默认 2）
  --max-body int           请求体上限（默认 65536 字节）
  --op-timeout duration    单次操作超时上限（默认 15m，需大于会话时长）
  --ws-idle-timeout duration WebSocket 空闲回收时间（默认 5m）
  --no-ws                  不注册 WebSocket 端点
  --version string         协议版本（默认 %s）
  --proxy string           HTTP 代理地址
  --timeout duration       单次请求超时（默认 30s）
  --session-timeout duration 单次会话时长上限（默认 10m）
  --log-level string       日志级别 debug/info/warn/error（默认 info）
`, protocol.DefaultVersion)
}
