package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// 退出码约定，见 §8。
const (
	exitOK       = 0
	exitBusiness = 1
	exitNetwork  = 2
	exitUsage    = 3
)

// usageError 表示参数用法错误。它是带分类的哨兵，因此 JSON 输出里 kind 会显示为 param，
// 退出码也会自动落到 3，无需在各处特判。
var usageError = &model.Error{Kind: model.KindParam, Sentinel: model.CodeUsage, Msg: "参数用法错误"}

// globalFlags 是所有子命令共用的参数。
type globalFlags struct {
	version        string
	proxy          string
	timeout        time.Duration
	sessionTimeout time.Duration
	logLevel       string
	titleBaseURL   string
	aimeURL        string
	chartBaseURL   string

	autoVersion   bool
	chartCacheDir string
	chartCacheTTL time.Duration
	noReuse       bool
}

// environment 是子命令的运行环境：出口、日志与已解析的通用参数。
type environment struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	flags globalFlags

	// exitCode 让子命令在不返回 error 的情况下指定退出码：probe 的自检未通过
	// 属于「命令执行成功但结论是故障」，结果照常写出，退出码如实反映故障类别。
	exitCode int
}

// newFlagSet 构造子命令的参数集，并挂上通用参数。
func (e *environment) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "用法: mai-arcade %s [参数]\n", name)
		fs.PrintDefaults()
	}

	fs.StringVar(&e.flags.version, "version", "", "协议版本（默认 "+protocol.DefaultVersion+"，可用 "+strings.Join(protocol.SupportedVersions(), "/")+"）")
	fs.StringVar(&e.flags.proxy, "proxy", "", "HTTP 代理地址")
	fs.DurationVar(&e.flags.timeout, "timeout", service.DefaultTimeout, "单次请求超时")
	fs.DurationVar(&e.flags.sessionTimeout, "session-timeout", service.DefaultSessionTimeout, "单次会话总时长上限，需小于机台 15 分钟硬超时")
	fs.StringVar(&e.flags.logLevel, "log-level", "info", "日志级别 debug/info/warn/error")
	fs.StringVar(&e.flags.titleBaseURL, "title-base-url", "", "标题服务器根地址（调试用）")
	fs.StringVar(&e.flags.aimeURL, "aime-url", "", "AimeDB 接口地址（调试用）")
	fs.StringVar(&e.flags.chartBaseURL, "chart-base-url", "", "查分器 API 根地址（调试用）")
	fs.BoolVar(&e.flags.autoVersion, "auto-version", false, "依次尝试 1.55 / 1.53，用探测到的可用版本")
	fs.StringVar(&e.flags.chartCacheDir, "chart-cache-dir", "", "曲目数据缓存目录，为空表示不缓存")
	fs.DurationVar(&e.flags.chartCacheTTL, "chart-cache-ttl", chart.DefaultCacheTTL, "曲目缓存有效期")
	fs.BoolVar(&e.flags.noReuse, "no-reuse", false, "关闭同一二维码在有效期内的结果复用")
	return fs
}

// parseFlags 解析参数，把用法错误统一成 usageError。
func (e *environment) parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fmt.Errorf("%w: %v", usageError, err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("%w: 多余的参数 %v（二维码请走 stdin）", usageError, rest)
	}
	return nil
}

// logger 按 --log-level 构造结构化日志器，输出到 stderr，与 stdout 的 JSON 结果分离。
func (e *environment) logger() (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(e.flags.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("%w: 未知日志级别 %q", usageError, e.flags.logLevel)
	}
	handler := slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: level})
	return slog.New(handler), nil
}

// newService 装配编排层。
func (e *environment) newService(logger *slog.Logger) (*service.Service, error) {
	return service.New(service.Options{
		Version:        e.flags.version,
		ProxyURL:       e.flags.proxy,
		Timeout:        e.flags.timeout,
		SessionTimeout: e.flags.sessionTimeout,
		TitleBaseURL:   e.flags.titleBaseURL,
		AimeURL:        e.flags.aimeURL,
		ChartBaseURL:   e.flags.chartBaseURL,
		ChartCacheDir:  e.flags.chartCacheDir,
		ChartCacheTTL:  e.flags.chartCacheTTL,
		DisableReuse:   e.flags.noReuse,
		Logger:         logger,
	})
}

// prepare 装配编排层，并在开启 --auto-version 时先探测可用版本。
func (e *environment) prepare(ctx context.Context, logger *slog.Logger) (*service.Service, error) {
	svc, err := e.newService(logger)
	if err != nil {
		return nil, err
	}
	if e.flags.autoVersion {
		if _, err := svc.DetectVersion(ctx); err != nil {
			return nil, err
		}
	}
	return svc, nil
}

// readSGID 从 stdin 读取二维码并校验。
//
// 二维码是账号唯一凭证，走 stdin 而非 argv，避免被 ps 看到。
func (e *environment) readSGID() (protocol.SGID, error) {
	raw, err := io.ReadAll(io.LimitReader(e.stdin, maxSGIDInput))
	if err != nil {
		return "", fmt.Errorf("%w: 读取二维码: %v", usageError, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", fmt.Errorf("%w: stdin 为空；二维码需通过管道传入，例如 echo \"$SGID\" | mai-arcade verify", usageError)
	}
	return protocol.NewSGID(string(raw))
}

// maxSGIDInput 限制 stdin 读取量：二维码只有 84 字符，多余内容一定是误传。
const maxSGIDInput = 4096

// output 是 stdout 的结构化结果信封，调用方无需解析人类可读文本。
type output struct {
	OK      bool         `json:"ok"`
	Command string       `json:"command"`
	Data    any          `json:"data,omitempty"`
	Error   *outputError `json:"error,omitempty"`
}

// outputError 是结构化错误。
//
// 字段分工：kind 决定退出码，code 精确到具体原因（调用方应优先按 code 分支），
// detail 是服务端返回的原始码（如 errorID=1），hint 是补救建议。
type outputError struct {
	Message  string `json:"message"`
	Kind     string `json:"kind"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// emit 把错误写成 JSON 信封，并返回退出码。
func emit(command string, env *environment, err error) int {
	code := exitCodeFor(err)
	out := output{OK: false, Command: command, Error: describeError(err, code)}
	encoder := json.NewEncoder(env.stdout)
	encoder.SetEscapeHTML(false)
	if encodeErr := encoder.Encode(out); encodeErr != nil {
		fmt.Fprintf(env.stderr, "写出结果失败: %v\n", encodeErr)
		return exitBusiness
	}
	return code
}

// emitData 写出成功结果；失败时返回错误，由调用方交给 emit 处理。
func emitData(env *environment, command string, data any) error {
	out := output{OK: true, Command: command, Data: data}
	encoder := json.NewEncoder(env.stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(out); err != nil {
		return fmt.Errorf("写出结果失败: %w", err)
	}
	return nil
}

// describeError 把错误展开成结构化字段。
func describeError(err error, code int) *outputError {
	out := &outputError{Message: err.Error(), Kind: kindName(err), ExitCode: code}
	if c, ok := model.CodeOf(err); ok {
		out.Code = string(c)
	}
	var me *model.Error
	if errors.As(err, &me) {
		out.Detail = me.Code
		out.Hint = me.Hint
	}
	return out
}

// kindName 返回错误的分类名。
func kindName(err error) string {
	var me *model.Error
	if !errors.As(err, &me) {
		return "unclassified"
	}
	return me.Kind.String()
}

// classifiedExitCode 取出已分类错误的退出码。
func classifiedExitCode(err error) (int, bool) {
	if errors.Is(err, usageError) {
		return exitUsage, true
	}
	var me *model.Error
	if errors.As(err, &me) {
		return me.Kind.ExitCode(), true
	}
	return 0, false
}
