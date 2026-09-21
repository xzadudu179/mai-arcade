package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// validToken 是满足长度要求的测试令牌。
const validToken = "0123456789abcdef0123456789abcdef"

// parse 是 parseConfig 的测试包装，忽略告警输出。
func parse(args ...string) (config, error) {
	return parseConfig(args, io.Discard)
}

// TestParseRejectsMissingTokenSource 断言不给令牌来源时拒绝启动。
//
// 本服务持有账号凭证与查分器写权限，无鉴权运行等于把两样东西都交出去。
func TestParseRejectsMissingTokenSource(t *testing.T) {
	_, err := parse()
	if err == nil {
		t.Fatal("缺少令牌来源应当报错")
	}
	if !strings.Contains(err.Error(), "必须指定令牌来源") {
		t.Errorf("错误信息应说明原因: %v", err)
	}
}

// TestParseRejectsMultipleTokenSources 断言同时给多个来源时拒绝启动，而不是猜一个。
func TestParseRejectsMultipleTokenSources(t *testing.T) {
	t.Setenv("MAI_TEST_TOKEN", validToken)

	_, err := parse("--token", validToken, "--token-env", "MAI_TEST_TOKEN")
	if err == nil {
		t.Fatal("同时给出多个令牌来源应当报错")
	}

	_, err = parse("--token-env", "MAI_TEST_TOKEN", "--token-file", "/tmp/x")
	if err == nil {
		t.Fatal("同时给出多个令牌来源应当报错")
	}
}

// TestParseReadsTokenFromEnv 断言能从环境变量读令牌（推荐方式）。
func TestParseReadsTokenFromEnv(t *testing.T) {
	t.Setenv("MAI_TEST_TOKEN", "  "+validToken+"  ")

	cfg, err := parse("--token-env", "MAI_TEST_TOKEN")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.token != validToken {
		t.Errorf("令牌 = %q, 期望去掉首尾空白后为 %q", cfg.token, validToken)
	}
	if cfg.tokenSource != tokenSourceEnv {
		t.Errorf("来源 = %q, 期望 %q", cfg.tokenSource, tokenSourceEnv)
	}
}

// TestParseRejectsEmptyEnvToken 断言环境变量为空时明确报错。
func TestParseRejectsEmptyEnvToken(t *testing.T) {
	t.Setenv("MAI_TEST_TOKEN", "   ")

	if _, err := parse("--token-env", "MAI_TEST_TOKEN"); err == nil {
		t.Fatal("环境变量为空应当报错")
	}
}

// TestParseReadsTokenFromFile 断言能从文件读令牌。
func TestParseReadsTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(validToken+"\n"), 0o600); err != nil {
		t.Fatalf("写令牌文件失败: %v", err)
	}

	cfg, err := parse("--token-file", path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.token != validToken {
		t.Errorf("令牌 = %q, 期望 %q", cfg.token, validToken)
	}
	if cfg.tokenSource != tokenSourceFile {
		t.Errorf("来源 = %q, 期望 %q", cfg.tokenSource, tokenSourceFile)
	}
}

// TestParseWarnsOnLooseFilePermissions 断言权限过宽的令牌文件会被提示。
func TestParseWarnsOnLooseFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(validToken), 0o644); err != nil {
		t.Fatalf("写令牌文件失败: %v", err)
	}

	var stderr bytes.Buffer
	if _, err := parseConfig([]string{"--token-file", path}, &stderr); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(stderr.String(), "权限") {
		t.Errorf("应对过宽的权限给出提示: %s", stderr.String())
	}
}

// TestParseRejectsMissingTokenFile 断言文件不存在时报错。
func TestParseRejectsMissingTokenFile(t *testing.T) {
	if _, err := parse("--token-file", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("文件不存在应当报错")
	}
}

// TestParseAcceptsShortFlagToken 断言直接给出的令牌可用，并标记来源为命令行参数。
func TestParseAcceptsShortFlagToken(t *testing.T) {
	cfg, err := parse("--token", validToken)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.tokenSource != tokenSourceArgv {
		t.Errorf("来源 = %q, 期望 %q", cfg.tokenSource, tokenSourceArgv)
	}
}

// TestParseRejectsWeakToken 断言过短的令牌被拒。
func TestParseRejectsWeakToken(t *testing.T) {
	if _, err := parse("--token", "short"); err == nil {
		t.Fatal("过短的令牌应当被拒绝")
	}
}

// TestParseDefaultsAreSafe 断言默认值是安全的那一侧。
func TestParseDefaultsAreSafe(t *testing.T) {
	cfg, err := parse("--token", validToken)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if cfg.allowWrite {
		t.Error("写操作默认必须关闭")
	}
	if !strings.HasPrefix(cfg.listen, "127.0.0.1:") {
		t.Errorf("默认监听 = %q, 期望只绑本机", cfg.listen)
	}
	// 版本会被归一化成具体取值：启动日志与自描述要显示真正生效的版本，
	// 而不是空串或「默认」这种需要二次解读的东西。
	if cfg.version != protocol.DefaultVersion {
		t.Errorf("默认版本 = %q, 期望 %q", cfg.version, protocol.DefaultVersion)
	}
}

// TestParseNormalizesVersion 断言版本号被归一化成生效值。
func TestParseNormalizesVersion(t *testing.T) {
	cfg, err := parse("--token", validToken)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.version == "" {
		t.Error("版本号应被归一化成具体取值，否则启动日志会显示空串")
	}

	if _, err := parse("--token", validToken, "--version", "9.99"); err == nil {
		t.Error("未知版本应当被拒绝")
	}
}

// TestParseRejectsBadNumericOptions 断言越界的数值参数被拒，而不是运行时才出问题。
func TestParseRejectsBadNumericOptions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"限频为负", []string{"--rate-burst", "-1"}},
		{"并发为 0", []string{"--concurrency", "0"}},
		{"请求体上限为 0", []string{"--max-body", "0"}},
		{"操作超时为 0", []string{"--op-timeout", "0s"}},
		{"空闲超时为 0", []string{"--ws-idle-timeout", "0s"}},
		{"限频窗口为 0", []string{"--rate-burst", "5", "--rate-interval", "0s"}},
		{"未知日志级别", []string{"--log-level", "nope"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--token", validToken}, tc.args...)
			if _, err := parse(args...); err == nil {
				t.Errorf("参数 %v 应当被拒绝", tc.args)
			}
		})
	}
}

// TestParseRequiresOperationTimeoutAboveSession 断言操作超时必须留在会话时长之上。
//
// 否则服务端会在机台会话收尾（登出）之前掐断请求，把账号留在「小黑屋」门口。
func TestParseRequiresOperationTimeoutAboveSession(t *testing.T) {
	_, err := parse("--token", validToken,
		"--session-timeout", "10m", "--op-timeout", "5m")
	if err == nil {
		t.Fatal("操作超时小于会话时长应当被拒绝")
	}
	if !strings.Contains(err.Error(), "登出") {
		t.Errorf("错误信息应说明后果: %v", err)
	}

	if _, err := parse("--token", validToken,
		"--session-timeout", "5m", "--op-timeout", "6m"); err != nil {
		t.Errorf("操作超时大于会话时长时应当通过: %v", err)
	}
}

// TestParseRejectsExtraArgs 断言多余的位置参数被拒。
func TestParseRejectsExtraArgs(t *testing.T) {
	if _, err := parse("--token", validToken, "extra"); err == nil {
		t.Fatal("多余的位置参数应当被拒绝")
	}
}

// TestParseHelp 断言 --help 走正常退出路径。
func TestParseHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseConfig([]string{"--help"}, &stderr)
	if err == nil {
		t.Fatal("--help 应返回哨兵错误以便主流程按成功退出")
	}
	if !strings.Contains(stderr.String(), "mai-arcade-server") {
		t.Errorf("帮助信息未输出: %s", stderr.String())
	}
}

// TestRunExitsWithUsageCode 断言启动失败返回参数错退出码。
func TestRunExitsWithUsageCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--token", "short"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "启动失败") {
		t.Errorf("应说明启动失败: %s", stderr.String())
	}
}

// TestRunHelpExitsZero 断言 --help 返回 0。
func TestRunHelpExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != exitOK {
		t.Errorf("退出码 = %d, 期望 0", code)
	}
}

// TestRunRejectsBadListenAddress 断言监听失败返回网络类退出码。
func TestRunRejectsBadListenAddress(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--token", validToken,
		"--listen", "256.256.256.256:99999",
	}, &stdout, &stderr)

	if code != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d", code, exitNetwork)
	}
}

// TestIsLoopback 断言回环地址判定。
func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8787", true},
		{"localhost:8787", true},
		{"[::1]:8787", true},
		{"0.0.0.0:8787", false},
		{"192.168.1.10:8787", false},
		{"bad-address", false},
	}

	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isLoopback(tc.addr); got != tc.want {
				t.Errorf("isLoopback(%q) = %v, 期望 %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestNewLoggerLevels 断言日志级别配置生效。
func TestNewLoggerLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", ""} {
		cfg := config{logLevel: level}
		if _, err := cfg.newLogger(io.Discard); err != nil {
			t.Errorf("级别 %q 应当被接受: %v", level, err)
		}
	}

	cfg := config{logLevel: "verbose"}
	if _, err := cfg.newLogger(io.Discard); err == nil {
		t.Error("未知级别应当报错")
	}
}

// TestConfigValidateDefaults 断言默认配置通过校验。
func TestConfigValidateDefaults(t *testing.T) {
	cfg := config{
		rateBurst:        defaultRateBurst,
		rateInterval:     defaultRateInterval,
		concurrency:      defaultConcurrency,
		maxBody:          64 << 10,
		operationTimeout: defaultOpTimeout,
		wsIdleTimeout:    defaultWSIdle,
		sessionTimeout:   10 * time.Minute,
		logLevel:         "info",
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("默认配置应当通过校验: %v", err)
	}
}
