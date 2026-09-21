// Package transport 集中管理 HTTP 细节：超时、代理、响应体大小上限与日志脱敏。
//
// 它不认识任何业务概念——不知道什么是 SGID、token 或成绩，只负责把字节发出去、把字节收回来。
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 传输层哨兵错误，供上层归类后映射为退出码。
var (
	// ErrNetwork 表示 TCP/TLS 建连失败：DNS、连接被拒、握手失败、超时。
	ErrNetwork = errors.New("网络不可达")

	// ErrTimeout 表示请求在超时时间内没有完成。
	ErrTimeout = errors.New("请求超时")
)

// maxResponseBytes 限制单个响应体的读取量，避免异常对端用超大响应耗尽内存。
const maxResponseBytes = 16 << 20

// Options 是构造 Client 的全部依赖，一律显式传入，不从环境变量读取。
type Options struct {
	// Timeout 是单次请求的总超时，零值表示 30 秒。
	Timeout time.Duration

	// ProxyURL 是 HTTP 代理地址，为空表示直连。
	ProxyURL string

	// Logger 用于记录脱敏后的请求摘要，nil 表示不记录。
	Logger *slog.Logger
}

// Client 是可复用的 HTTP 客户端，自带代理与超时配置。
type Client struct {
	hc     *http.Client
	logger *slog.Logger
}

// New 构造 Client；ProxyURL 非法时返回错误而不是静默降级为直连。
func New(opts Options) (*Client, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
		// 机台要求请求头里 Accept-Encoding 为空以禁用压缩协商。
		// 只把该头设成空串不够：Go 的 transport 判断「值为空」等价于「未设置」，
		// 于是会自动补上 gzip 并透明解压响应——那样请求头就不符合协议了。
		DisableCompression: true,
	}
	if opts.ProxyURL == "" {
		// 显式直连：避免本机 HTTP_PROXY 环境变量把机台请求带进代理。
		tr.Proxy = nil
	} else {
		u, err := url.Parse(opts.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析代理地址 %q: %w", opts.ProxyURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("代理地址 %q 缺少 scheme 或 host", opts.ProxyURL)
		}
		tr.Proxy = http.ProxyURL(u)
	}

	return &Client{
		hc: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// 机台的 api_hash 在路径里，任何重定向都会让 hash 失配，因此一律不跟随。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: opts.Logger,
	}, nil
}

// Response 是收发结果：只保留状态码、响应头与响应体。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// ErrEmpty 判定响应体为空。机台出口 IP 被阻断时正是「HTTP 200 + 0 字节」，
// 上层据此区分「网络不通」与「被华立阻断」。
func (r *Response) ErrEmpty() bool { return len(r.Body) == 0 }

// Post 发送 POST 请求并读回完整响应体。
//
// headers 中的空字符串值会被真实写出（机台要求显式声明 Accept-Encoding 为空以禁用压缩）。
func (c *Client) Post(ctx context.Context, rawURL string, headers map[string]string, body []byte) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s: %w", redactURL(rawURL), err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

// Get 发送 GET 请求并读回完整响应体。
func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s: %w", redactURL(rawURL), err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

func (c *Client) do(req *http.Request) (*Response, error) {
	started := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, fmt.Errorf("%w: %s", ErrTimeout, req.URL.Host)
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrNetwork, req.URL.Host, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取响应体: %v", ErrNetwork, err)
	}

	if c.logger != nil {
		c.logger.Debug("http 完成",
			"主机", req.URL.Host,
			"路径段数", strings.Count(req.URL.Path, "/"),
			"状态码", resp.StatusCode,
			"响应字节", len(body),
			"耗时", time.Since(started).Round(time.Millisecond).String())
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// HostReachable 只做 TCP+TLS 建连，用于把「网络不通」从「被阻断」里摘出来。
//
// 它故意不发送任何请求：机台对路径不敏感，只要能建连却拿不到响应，就是出口 IP 被拦。
func (c *Client) HostReachable(ctx context.Context, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return fmt.Errorf("构造探测请求: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return fmt.Errorf("%w: %s", ErrTimeout, req.URL.Host)
		}
		return fmt.Errorf("%w: %s: %v", ErrNetwork, req.URL.Host, err)
	}
	resp.Body.Close()
	return nil
}

// Redact 把敏感值截成「前 keep 位 + 长度」，用于日志。
//
// 硬性约束：二维码与 token 全文不得出现在任何日志里。keep 超过原值长度时按原值长度处理——
// 调用方明确要了这么多位，静默返回空串会让它无从发现自己的参数写错了。
func Redact(value string, keep int) string {
	if value == "" {
		return "<空>"
	}
	if keep < 0 {
		keep = 0
	}
	if keep > len(value) {
		keep = len(value)
	}
	return fmt.Sprintf("%s…(len=%d)", value[:keep], len(value))
}

// redactURL 去掉 query 与 userinfo：两者都可能夹带凭证。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<无法解析的 URL>"
	}
	u.RawQuery = ""
	u.User = nil
	return u.String()
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	var t timeout
	return errors.As(err, &t) && t.Timeout()
}
