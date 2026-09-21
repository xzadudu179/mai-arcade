package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newClient 构造指向测试服务器的客户端。
func newClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("构造 Client 失败: %v", err)
	}
	return c
}

// TestRedact 断言脱敏只露出前若干位并带上长度。
func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		value string
		keep  int
		want  string
	}{
		{"正常截断", "SGWCMAID260920205100ABCDEF", 8, "SGWCMAID…(len=26)"},
		{"token 前 4 位", "abcdef123456", 4, "abcd…(len=12)"},
		{"空值", "", 8, "<空>"},
		{"保留位数超过长度时全部保留", "short", 99, "short…(len=5)"},
		{"非正保留位数视为不保留", "abc", 0, "…(len=3)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.value, tc.keep); got != tc.want {
				t.Errorf("Redact(%q, %d) = %q, 期望 %q", tc.value, tc.keep, got, tc.want)
			}
		})
	}
}

// TestRedactNeverRevealsTail 断言脱敏输出不含原值的后半段。
func TestRedactNeverRevealsTail(t *testing.T) {
	secret := "SGWCMAID260920205100" + strings.Repeat("AB", 32)
	redacted := Redact(secret, 8)
	if strings.Contains(redacted, strings.Repeat("AB", 32)) {
		t.Errorf("脱敏输出泄露了后半段: %s", redacted)
	}
}

// TestPostDeliversHeadersVerbatim 断言请求头被原样送达，含显式空值。
func TestPostDeliversHeadersVerbatim(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	client := newClient(t, Options{})
	resp, err := client.Post(context.Background(), server.URL, map[string]string{
		"X-Custom":        "value",
		"Accept-Encoding": "",
	}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Post 报错: %v", err)
	}
	if string(resp.Body) != "ok" {
		t.Errorf("响应体 = %q, 期望 ok", resp.Body)
	}
	if got.Get("X-Custom") != "value" {
		t.Errorf("自定义头未送达: %v", got)
	}
	// 空值头必须真实存在，机台要求显式声明禁用压缩。
	if values, ok := got["Accept-Encoding"]; !ok || len(values) != 1 || values[0] != "" {
		t.Errorf("Accept-Encoding = %v, 期望单个空串", values)
	}
}

// TestResponseErrEmpty 断言空响应体被识别出来。
//
// 「HTTP 200 + 0 字节」是出口 IP 被华立阻断的特征，上层依赖这个判定。
func TestResponseErrEmpty(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"空响应体", "", true},
		{"非空响应体", "x", false},
		{"空白响应体不算空", " ", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &Response{Body: []byte(tc.body)}
			if got := resp.ErrEmpty(); got != tc.want {
				t.Errorf("ErrEmpty() = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// TestNetworkFailureIsClassified 断言连不上时被归为网络错误。
func TestNetworkFailureIsClassified(t *testing.T) {
	// 指向一个已关闭的端口。
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	client := newClient(t, Options{Timeout: 2 * time.Second})
	_, err := client.Post(context.Background(), url, nil, []byte(`{}`))
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("错误 = %v, 期望归为 %v", err, ErrNetwork)
	}
}

// TestTimeoutIsClassified 断言超时被单独归类。
func TestTimeoutIsClassified(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	client := newClient(t, Options{Timeout: 100 * time.Millisecond})
	_, err := client.Get(context.Background(), server.URL, nil)
	if err == nil {
		t.Fatal("超时应当报错")
	}
	if !errors.Is(err, ErrTimeout) && !errors.Is(err, ErrNetwork) {
		t.Errorf("错误 = %v, 期望归为超时或网络", err)
	}
}

// TestContextCancelIsClassified 断言调用方取消也被归为网络错误。
func TestContextCancelIsClassified(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newClient(t, Options{})
	_, err := client.Get(ctx, server.URL, nil)
	if err == nil {
		t.Fatal("取消后应当报错")
	}
	if !errors.Is(err, ErrNetwork) && !errors.Is(err, ErrTimeout) {
		t.Errorf("错误 = %v, 期望归为网络类", err)
	}
}

// TestRedirectsAreNotFollowed 断言不跟随重定向。
//
// api_hash 在路径里，跟随重定向会让 hash 与路径失配，请求必然失败。
func TestRedirectsAreNotFollowed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应跟随到重定向目标")
	}))
	t.Cleanup(target.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(server.Close)

	client := newClient(t, Options{})
	resp, err := client.Get(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("Get 报错: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("状态码 = %d, 期望原样返回 302", resp.StatusCode)
	}
}

// TestRejectsBadProxyURL 断言非法代理地址在构造时就报错，而不是静默直连。
//
// 静默直连会让用户以为走了代理，实际上用的是被阻断的出口 IP，排查时会绕远路。
func TestRejectsBadProxyURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"缺少 scheme", "127.0.0.1:8080"},
		{"只有 scheme", "http://"},
		{"无法解析", "://bad"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Options{ProxyURL: tc.url}); err == nil {
				t.Errorf("代理地址 %q 应当被拒绝", tc.url)
			}
		})
	}
}

// TestAcceptsValidProxyURL 断言合法代理地址被接受。
func TestAcceptsValidProxyURL(t *testing.T) {
	if _, err := New(Options{ProxyURL: "http://127.0.0.1:8080"}); err != nil {
		t.Errorf("合法代理地址不应报错: %v", err)
	}
}

// TestHostReachable 断言建连探测能区分「通」与「不通」。
func TestHostReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	client := newClient(t, Options{})
	if err := client.HostReachable(context.Background(), server.URL); err != nil {
		t.Errorf("可达主机不应报错: %v", err)
	}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	if err := client.HostReachable(context.Background(), url); err == nil {
		t.Error("不可达主机应当报错")
	}
}

// TestRedactURLStripsCredentials 断言日志里的 URL 不含 query 与 userinfo。
func TestRedactURLStripsCredentials(t *testing.T) {
	got := redactURL("https://user:pass@example.com/path?token=secret")
	if strings.Contains(got, "secret") || strings.Contains(got, "pass") {
		t.Errorf("URL 脱敏不彻底: %s", got)
	}
	if !strings.Contains(got, "example.com") {
		t.Errorf("应保留主机名: %s", got)
	}
}
