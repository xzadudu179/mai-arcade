// Command mai-arcade 通过玩家二维码读取舞萌 DX 机台账号的完整成绩（含 FC/FS）并同步到查分器。
//
// 本文件只做三件事：解析参数、装配依赖、分发子命令，不含任何业务逻辑。
// 新增子命令 = 新增 cmd/mai-arcade/<name>.go 并在 commands 表里加一行。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// commandFunc 是子命令的签名。
type commandFunc func(context.Context, *environment, []string) error

// commands 是子命令注册表：这里是唯一的「有哪些命令」的事实来源。
var commands = map[string]commandFunc{
	"probe":   runProbe,
	"verify":  runVerify,
	"sync":    runSync,
	"profile": runProfile,
	"b50":     runB50,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run 是可测试的入口：解析子命令、装配环境、分发并返回退出码。
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	env := &environment{stdin: stdin, stdout: stdout, stderr: stderr}

	if len(args) == 0 {
		printUsage(stderr)
		return emit("", env, fmt.Errorf("%w: 需要指定子命令", usageError))
	}

	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		printUsage(stdout)
		return exitOK
	}

	command, ok := commands[name]
	if !ok {
		printUsage(stderr)
		return emit(name, env, fmt.Errorf("%w: 未知子命令 %q", usageError, name))
	}

	// Ctrl-C 也要走正常路径：取消上下文后会话的 defer 仍会尝试登出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := command(ctx, env, args[1:]); err != nil {
		return emit(name, env, err)
	}
	return env.exitCode
}

// printUsage 打印命令一览。
func printUsage(w io.Writer) {
	fmt.Fprint(w, `mai-arcade —— 舞萌 DX 机台协议工具

用法:
  mai-arcade <命令> [参数]

命令:
  probe     连通性自检，无需凭证；区分网络不通 / IP 被阻断 / 参数错 / 业务错
  verify    读 stdin 的二维码，拉取全量成绩并打印字段结构（脱敏，不上传）
  sync      读 stdin 的二维码，拉取全量成绩并同步到查分器
  profile   读 stdin 的二维码，打印账号资料（头像 / 姓名框 / 牌子 / 称号 / 评级）
  b50       读 stdin 的二维码，计算 b50 与 rating

通用参数:
  --version string    协议版本，取值见下
  --proxy string      HTTP 代理地址
  --timeout duration  单次请求超时（默认 30s）
  --log-level string  日志级别 debug/info/warn/error（默认 info）
  --help              查看子命令参数

协议版本: `+strings.Join(protocol.SupportedVersions(), " ")+`（默认 `+protocol.DefaultVersion+`）

示例:
  mai-arcade probe
  echo "$SGID" | mai-arcade verify
  echo "$SGID" | mai-arcade sync --fish-token "$FISH_TOKEN"

注意: 二维码只经 stdin 传入，不要写在命令行参数里——ps 会暴露 argv。
`)
}

// exitCodeFor 把错误翻译成 §8 约定的退出码：0 成功 / 1 业务失败 / 2 网络或阻断 / 3 参数错。
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	code, ok := classifiedExitCode(err)
	if ok {
		return code
	}
	// 未分类的错误按业务失败处理：它已经越过参数与网络校验，属于「跑起来了但没成功」。
	return exitBusiness
}
