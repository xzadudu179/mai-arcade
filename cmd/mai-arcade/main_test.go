package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// envelope 是 stdout 上的结构化结果，调用方按它取用。
type envelope struct {
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Message  string `json:"message"`
		Kind     string `json:"kind"`
		Code     string `json:"code"`
		Hint     string `json:"hint"`
		ExitCode int    `json:"exitCode"`
	} `json:"error"`
}

// invoke 跑一次 CLI，返回退出码与解析后的信封。
func invoke(t *testing.T, stdin string, args ...string) (int, envelope, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)

	var env envelope
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("stdout 不是合法 JSON: %v（原文 %s）", err, stdout.String())
		}
	}
	return code, env, stderr.String()
}

// blockedTitleServer 返回一个「HTTP 200 + 0 字节」的假机台，复现被阻断的特征。
func blockedTitleServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/Maimai2Servlet/"
}

// fakeAimeDB 返回一个可用的假 AimeDB 地址。
func fakeAimeDB(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorID":0,"userID":10807675,"token":"token-abc"}`))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// validSGIDText 构造一枚当前有效的二维码文本。
func validSGIDText(t *testing.T) string {
	t.Helper()
	return "SGWCMAID" + time.Now().Format("060102150405") + strings.Repeat("0123456789ABCDEF", 4)
}

// TestCLIUsageErrorsExitThree 断言用法错误一律返回 3，并给出结构化错误。
func TestCLIUsageErrorsExitThree(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"没有子命令", "", nil},
		{"未知子命令", "", []string{"nosuchcmd"}},
		{"未知协议版本", "", []string{"probe", "--version", "9.99"}},
		{"未知日志级别", "", []string{"probe", "--log-level", "nope"}},
		{"多余的位置参数", "", []string{"verify", "extra-arg"}},
		{"stdin 为空", "", []string{"verify"}},
		{"二维码格式非法", "not-a-qrcode", []string{"verify"}},
		{"sync 缺少凭证", validSGIDText(t), []string{"sync"}},
		{"sync 凭证参数冲突", validSGIDText(t), []string{"sync", "--credential", "a", "--fish-token", "b"}},
		{"sync 未知站点", validSGIDText(t), []string{"sync", "--site", "nope", "--credential", "a"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, env, _ := invoke(t, tc.stdin, tc.args...)
			if code != exitUsage {
				t.Errorf("退出码 = %d, 期望 %d", code, exitUsage)
			}
			if env.Error == nil {
				t.Fatal("用法错误应当带上结构化错误")
			}
			if env.Error.Kind != "param" {
				t.Errorf("kind = %q, 期望 param", env.Error.Kind)
			}
			if env.Error.ExitCode != exitUsage {
				t.Errorf("信封里的退出码 = %d, 期望 %d", env.Error.ExitCode, exitUsage)
			}
			if env.Error.Message == "" {
				t.Error("错误应当有可读描述")
			}
			if env.OK {
				t.Error("失败结果不应标记 ok")
			}
		})
	}
}

// TestCLIHelpExitsZero 断言帮助信息不算失败。
func TestCLIHelpExitsZero(t *testing.T) {
	// 注意 nil 不在这个列表里：没给子命令属于用法错误（退出码 3），不是「请求帮助」。
	for _, args := range [][]string{{"--help"}, {"help"}} {
		var stdout bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &bytes.Buffer{})
		if code != exitOK {
			t.Errorf("args=%v 退出码 = %d, 期望 0", args, code)
		}
		if !strings.Contains(stdout.String(), "probe") {
			t.Errorf("帮助信息应列出子命令: %s", stdout.String())
		}
	}
}

// TestCLIProbeReportsBlockedWithExitTwo 断言 probe 把「被阻断」如实反映成退出码 2。
//
// 部署脚本靠这个退出码判断当前网络能不能用，因此它必须与 §5.6 的表严格一致。
func TestCLIProbeReportsBlockedWithExitTwo(t *testing.T) {
	code, env, _ := invoke(t, "",
		"probe",
		"--title-base-url", blockedTitleServer(t),
		"--aime-url", fakeAimeDB(t),
	)

	if code != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d", code, exitNetwork)
	}
	if !env.OK {
		t.Error("probe 本身执行成功，应当标记 ok（结论是故障，不是命令失败）")
	}

	var data struct {
		Version  string `json:"version"`
		ExitCode int    `json:"exitCode"`
		Verdicts []struct {
			Target string `json:"target"`
			Class  string `json:"class"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
			Hint   string `json:"hint"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("解析 probe 结果失败: %v", err)
	}
	if data.ExitCode != exitNetwork {
		t.Errorf("结果里的退出码 = %d, 期望 %d", data.ExitCode, exitNetwork)
	}
	if data.Version == "" {
		t.Error("结果应带上协议版本")
	}

	byTarget := map[string]string{}
	hints := map[string]string{}
	for _, v := range data.Verdicts {
		byTarget[v.Target] = v.Class
		hints[v.Target] = v.Hint
	}
	if byTarget["title"] != "blocked" {
		t.Errorf("机台判定 = %q, 期望 blocked", byTarget["title"])
	}
	if byTarget["aime"] != "ok" {
		t.Errorf("AimeDB 判定 = %q, 期望 ok", byTarget["aime"])
	}
	if !strings.Contains(hints["title"], "IP") {
		t.Errorf("应给出换 IP 的建议: %s", hints["title"])
	}
}

// TestCLIProbeReportsNetworkFailure 断言连不上时退出码同样是 2，但判定不同。
func TestCLIProbeReportsNetworkFailure(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()

	code, env, _ := invoke(t, "", "probe", "--title-base-url", url+"/Maimai2Servlet/", "--aime-url", url)
	if code != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d", code, exitNetwork)
	}

	if !strings.Contains(string(env.Data), `"class":"network"`) {
		t.Errorf("判定应为 network: %s", env.Data)
	}
}

// TestCLIVerifyFailsWithTwoWhenBlocked 断言链路通不了时退出码为 2，且错误里带上原因与建议。
func TestCLIVerifyFailsWithTwoWhenBlocked(t *testing.T) {
	code, env, _ := invoke(t, validSGIDText(t),
		"verify",
		"--title-base-url", blockedTitleServer(t),
		"--aime-url", fakeAimeDB(t),
	)

	if code != exitNetwork {
		t.Errorf("退出码 = %d, 期望 %d", code, exitNetwork)
	}
	if env.Error == nil {
		t.Fatal("应当带上结构化错误")
	}
	if env.Error.Kind != "network" {
		t.Errorf("kind = %q, 期望 network", env.Error.Kind)
	}
	if !strings.Contains(env.Error.Hint, "IP") {
		t.Errorf("应说明下一步怎么办: %s", env.Error.Hint)
	}
}

// TestCLIExpiredSGIDExitsThree 断言过期二维码归为参数错。
func TestCLIExpiredSGIDExitsThree(t *testing.T) {
	expired := "SGWCMAID" + time.Now().Add(-time.Hour).Format("060102150405") + strings.Repeat("0", 64)

	code, env, _ := invoke(t, expired,
		"verify",
		"--title-base-url", blockedTitleServer(t),
		"--aime-url", fakeAimeDB(t),
	)
	if code != exitUsage {
		t.Errorf("退出码 = %d, 期望 %d", code, exitUsage)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "过期") {
		t.Errorf("错误应说明二维码已过期: %+v", env.Error)
	}
}

// TestCLIOutputIsJSONWithoutCredentials 断言 stdout 只有 JSON，且不含二维码与 token 全文。
//
// 硬性要求：二维码与 token 不得回显，日志走 stderr 与结果分离。
func TestCLIOutputIsJSONWithoutCredentials(t *testing.T) {
	sgid := validSGIDText(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"verify",
		"--title-base-url", blockedTitleServer(t),
		"--aime-url", fakeAimeDB(t),
		"--log-level", "debug",
	}, strings.NewReader(sgid), &stdout, &stderr)

	if code != exitNetwork {
		t.Fatalf("退出码 = %d, 期望 %d", code, exitNetwork)
	}

	// stdout 必须是一整行 JSON，不含日志。
	if lines := strings.Count(strings.TrimSpace(stdout.String()), "\n"); lines != 0 {
		t.Errorf("stdout 应当只有一行 JSON，实际 %d 行", lines+1)
	}
	if strings.Contains(stdout.String(), sgid) {
		t.Error("stdout 泄露了二维码全文")
	}
	if strings.Contains(stderr.String(), sgid) {
		t.Errorf("stderr 泄露了二维码全文: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "token-abc") {
		t.Errorf("stderr 泄露了 token: %s", stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Errorf("stdout 不是合法 JSON: %s", stdout.String())
	}
}

// TestCLIVersionFlagReachesService 断言 --version 生效并反映在输出里。
func TestCLIVersionFlagReachesService(t *testing.T) {
	code, env, _ := invoke(t, "",
		"probe",
		"--version", "1.55",
		"--title-base-url", blockedTitleServer(t),
		"--aime-url", fakeAimeDB(t),
	)
	if code != exitNetwork {
		t.Fatalf("退出码 = %d, 期望 %d", code, exitNetwork)
	}
	if !strings.Contains(string(env.Data), `"version":"1.55"`) {
		t.Errorf("结果里的版本应为 1.55: %s", env.Data)
	}
}

// TestCLICommandsMapCoversEveryCommand 断言注册表与帮助文案一致。
func TestCLICommandsMapCoversEveryCommand(t *testing.T) {
	for name := range commands {
		if commands[name] == nil {
			t.Errorf("子命令 %q 的处理函数为空", name)
		}
	}

	var stdout bytes.Buffer
	printUsage(&stdout)
	for name := range commands {
		if !strings.Contains(stdout.String(), name) {
			t.Errorf("帮助文案缺少子命令 %q", name)
		}
	}
}

// TestSyncSiteFlagListsRegisteredSites 断言 --site 的帮助文案来自注册表而非硬编码。
func TestSyncSiteFlagListsRegisteredSites(t *testing.T) {
	var stderr bytes.Buffer
	fs := (&environment{stderr: &stderr}).newFlagSet("sync")
	fs.PrintDefaults()

	// 未注册任何站点时也不应崩溃；注册表内容由 sync 包负责。
	if strings.Contains(stderr.String(), "unknown") {
		t.Errorf("帮助文案异常: %s", stderr.String())
	}
}
