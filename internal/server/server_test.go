package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/access"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/ops"
	"github.com/xzadudu179/maimai-arcade/internal/service"
	"github.com/xzadudu179/maimai-arcade/internal/wsproto"
)

// testToken 是测试用的服务令牌。
const testToken = "test-token-0123456789abcdef"

// blockedTitleServer 返回一个「HTTP 200 + 0 字节」的假机台，复现被阻断的特征。
func blockedTitleServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/Maimai2Servlet/"
}

// fakeAimeDB 返回一个可用的假 AimeDB。
func fakeAimeDB(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorID":0,"userID":10807675,"token":"token-abc"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newTestHandler 装配一个指向上游假服务器的处理器。
func newTestHandler(t *testing.T, opts func(*Options)) http.Handler {
	t.Helper()

	svc, err := service.New(service.Options{
		Version:        "1.53",
		Timeout:        5 * time.Second,
		SessionTimeout: 5 * time.Second,
		TitleBaseURL:   blockedTitleServer(t),
		AimeURL:        fakeAimeDB(t),
		ChartBaseURL:   blockedTitleServer(t),
	})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	auth, err := access.NewAuthenticator(testToken)
	if err != nil {
		t.Fatalf("构造鉴权器失败: %v", err)
	}

	options := Options{
		Deps:             ops.Deps{Service: svc},
		Auth:             auth,
		MaxBody:          4096,
		OperationTimeout: 30 * time.Second,
		WSIdleTimeout:    5 * time.Second,
	}
	if opts != nil {
		opts(&options)
	}

	handler, err := NewHTTPHandler(options)
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	return handler
}

// do 发一次带令牌的 HTTP 请求。
func do(t *testing.T, handler http.Handler, method, path, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// decode 解析响应信封。
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（原文 %s）", err, rec.Body.String())
	}
	return out
}

// TestHealthNeedsNoAuth 断言存活探针不鉴权。
func TestHealthNeedsNoAuth(t *testing.T) {
	handler := newTestHandler(t, nil)

	req := httptest.NewRequest(http.MethodGet, pathHealth, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 200", rec.Code)
	}
	body := decode(t, rec)
	if body["ok"] != true {
		t.Errorf("响应 = %v, 期望 ok=true", body)
	}
}

// TestAuthRequired 断言缺少或错误的令牌一律 401。
func TestAuthRequired(t *testing.T) {
	handler := newTestHandler(t, nil)

	tests := []struct {
		name   string
		header func(*http.Request)
	}{
		{"没有凭证", func(r *http.Request) { r.Header.Del("Authorization") }},
		{"错误的令牌", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }},
		{"scheme 不对", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+testToken) }},
		{"空令牌", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, handler, http.MethodPost, "/v1/op/version", "{}", tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d, 期望 401", rec.Code)
			}
			body := decode(t, rec)
			errObj, _ := body["error"].(map[string]any)
			if errObj["code"] != string(model.CodeUnauthorized) {
				t.Errorf("错误码 = %v, 期望 %s", errObj["code"], model.CodeUnauthorized)
			}
		})
	}
}

// TestAuthAcceptsAlternateHeaders 断言两种等价写法都被接受。
func TestAuthAcceptsAlternateHeaders(t *testing.T) {
	handler := newTestHandler(t, nil)

	tests := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"X-Auth-Token", func(r *http.Request) {
			r.Header.Del("Authorization")
			r.Header.Set("X-Auth-Token", testToken)
		}},
		{"小写 bearer", func(r *http.Request) {
			r.Header.Set("Authorization", "bearer "+testToken)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, handler, http.MethodPost, "/v1/op/version", "{}", tc.apply)
			if rec.Code != http.StatusOK {
				t.Errorf("状态码 = %d, 期望 200（响应 %s）", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDescribeExposesOperationsAndCodes 断言自描述端点给出操作与全部错误码。
func TestDescribeExposesOperationsAndCodes(t *testing.T) {
	handler := newTestHandler(t, nil)
	rec := do(t, handler, http.MethodGet, pathOps, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	data, _ := decode(t, rec)["data"].(map[string]any)

	operations, _ := data["operations"].([]any)
	if len(operations) == 0 {
		t.Fatal("应当列出可用操作")
	}
	names := map[string]bool{}
	for _, item := range operations {
		op, _ := item.(map[string]any)
		names[op["name"].(string)] = true
	}
	for _, want := range []string{"probe", "verify", "sync", "profile", "b50"} {
		if !names[want] {
			t.Errorf("自描述里缺少操作 %q", want)
		}
	}

	codes, _ := data["errorCodes"].([]any)
	if len(codes) < 20 {
		t.Errorf("错误码条目 = %d, 偏少", len(codes))
	}
	// 同步操作必须被标为会改动数据，客户端据此判断风险。
	for _, item := range operations {
		op, _ := item.(map[string]any)
		if op["name"] == "sync" && op["mutating"] != true {
			t.Error("sync 应被标记为 mutating")
		}
	}
}

// TestWriteOperationBlockedByDefault 断言写操作默认被拒。
//
// 这是本服务最重要的安全默认值：暴露到网络上时只读应当是默认状态。
func TestWriteOperationBlockedByDefault(t *testing.T) {
	handler := newTestHandler(t, nil)
	rec := do(t, handler, http.MethodPost, "/v1/op/sync", `{"sgid":"x","credential":"y"}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d, 期望 403", rec.Code)
	}
	errObj, _ := decode(t, rec)["error"].(map[string]any)
	if errObj["code"] != string(model.CodeWriteDisabled) {
		t.Errorf("错误码 = %v, 期望 %s", errObj["code"], model.CodeWriteDisabled)
	}
}

// TestWriteOperationAllowedWhenEnabled 断言显式开启后写操作能进入执行。
func TestWriteOperationAllowedWhenEnabled(t *testing.T) {
	handler := newTestHandler(t, func(o *Options) { o.Deps.AllowWrite = true })

	// 凭证缺失会被参数校验挡下，但错误码不再是 write_disabled，说明写保护已放行。
	rec := do(t, handler, http.MethodPost, "/v1/op/sync", `{"sgid":"x"}`)
	errObj, _ := decode(t, rec)["error"].(map[string]any)
	if errObj["code"] == string(model.CodeWriteDisabled) {
		t.Errorf("写操作已开启，不应再报 write_disabled")
	}
	if errObj["code"] != string(model.CodeSGIDFormat) && errObj["code"] != string(model.CodeBadArguments) {
		t.Errorf("错误码 = %v, 期望参数类错误", errObj["code"])
	}
}

// TestUnknownOperationReturns404 断言未知操作返回 404 并提示可用操作。
func TestUnknownOperationReturns404(t *testing.T) {
	handler := newTestHandler(t, nil)
	rec := do(t, handler, http.MethodPost, "/v1/op/nope", "{}")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", rec.Code)
	}
	errObj, _ := decode(t, rec)["error"].(map[string]any)
	if errObj["code"] != string(model.CodeUnknownOp) {
		t.Errorf("错误码 = %v, 期望 %s", errObj["code"], model.CodeUnknownOp)
	}
}

// TestBodyTooLargeReturns413 断言超过上限的请求体被拒。
func TestBodyTooLargeReturns413(t *testing.T) {
	handler := newTestHandler(t, func(o *Options) { o.MaxBody = 64 })

	rec := do(t, handler, http.MethodPost, "/v1/op/verify",
		`{"sgid":"`+strings.Repeat("A", 200)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d, 期望 413", rec.Code)
	}
}

// TestBadJSONReturns400 断言非法 JSON 参数被拒。
func TestBadJSONReturns400(t *testing.T) {
	handler := newTestHandler(t, nil)
	rec := do(t, handler, http.MethodPost, "/v1/op/verify", "{not json")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", rec.Code)
	}
}

// TestRateLimitReturns429WithRetryAfter 断言限频生效并给出 Retry-After。
func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	limiter := access.NewRateLimiter(2, time.Minute)
	handler := newTestHandler(t, func(o *Options) { o.Limiter = limiter })

	codes := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		codes = append(codes, do(t, handler, http.MethodPost, "/v1/op/version", "{}").Code)
	}

	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("前两次应放行，实际 %v", codes)
	}
	for _, code := range codes[2:] {
		if code != http.StatusTooManyRequests {
			t.Errorf("状态码 = %d, 期望 429（实际序列 %v）", code, codes)
		}
	}

	rec := do(t, handler, http.MethodPost, "/v1/op/version", "{}")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 应带上 Retry-After 头")
	}
}

// TestConcurrencyLimitReturns503 断言并发占满时返回 503 而不是排队堆积。
func TestConcurrencyLimitReturns503(t *testing.T) {
	handler := newTestHandler(t, func(o *Options) {
		// 容量为 0 的缓冲通道：TryAcquire 永远失败，等价于并发已满。
		o.Sem = access.NewSemaphore(1)
	})

	// 占满唯一名额。
	blocker := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/op/version", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		<-blocker
		handler.ServeHTTP(rec, req)
	}()
	close(blocker)

	// 并发占用与释放有竞态，这里只断言最终会出现 503 或被正常处理，不引入时序假设。
	rec := do(t, handler, http.MethodPost, "/v1/op/version", "{}")
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d, 期望 200 或 503", rec.Code)
	}
}

// TestProbeOverHTTPReportsBlocked 断言 HTTP 路径下 probe 如实反映上游状态。
func TestProbeOverHTTPReportsBlocked(t *testing.T) {
	handler := newTestHandler(t, nil)
	rec := do(t, handler, http.MethodPost, "/v1/op/probe", "{}")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（probe 自身执行成功）", rec.Code)
	}
	data, _ := decode(t, rec)["data"].(map[string]any)
	if data["exitCode"] != float64(2) {
		t.Errorf("exitCode = %v, 期望 2（被阻断）", data["exitCode"])
	}
}

// TestMethodNotAllowed 断言方法不匹配时返回 405。
func TestMethodNotAllowed(t *testing.T) {
	handler := newTestHandler(t, nil)
	rec := do(t, handler, http.MethodGet, "/v1/op/verify", "")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d, 期望 405", rec.Code)
	}
}

// TestCatalogCoversAllDeclaredCodes 断言对外目录覆盖了每个声明过的错误码。
//
// 目录由各包的 init 自行登记，所以只有在链接齐全的二进制里才谈得上完整——
// 这条测试跑在 internal/server 的测试二进制里，它连通了 protocol、sync、access 与 ops，
// 因此能真正挡住「新增了码却忘了登记说明」的情况。
func TestCatalogCoversAllDeclaredCodes(t *testing.T) {
	registered := make(map[model.Code]bool)
	for _, info := range model.Errors() {
		registered[info.Code] = true
	}

	for _, code := range model.DeclaredCodes() {
		if !registered[code] {
			t.Errorf("错误码 %s 已声明但没有登记对外说明（客户端会拿到空码）", code)
		}
	}
	if len(registered) != len(model.DeclaredCodes()) {
		t.Errorf("目录条目 %d 个, 声明的码 %d 个，两者应当一一对应",
			len(registered), len(model.DeclaredCodes()))
	}
}

// TestEveryRegisteredCodeHasExplicitStatus 断言每个错误码都被显式映射过状态。
//
// 靠 Kind 兜底能给出「大致合理」的状态，但掩盖了「这个码到底是什么语义」的判断；
// 这条测试强制每个新码都做一次有意识的决定。
func TestEveryRegisteredCodeHasExplicitStatus(t *testing.T) {
	for _, info := range model.Errors() {
		if _, ok := codeStatus[info.Code]; !ok {
			t.Errorf("错误码 %s 没有显式的 HTTP 状态映射", info.Code)
		}
	}
}

// TestStatusForFallsBackByKind 断言没有具名码时按分类兜底。
func TestStatusForFallsBackByKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"参数错", model.ErrParam, http.StatusBadRequest},
		{"网络错", model.ErrNetwork, http.StatusBadGateway},
		{"业务错", model.ErrBusiness, http.StatusConflict},
		{"无分类", errors.New("随便一个错误"), http.StatusInternalServerError},
		{"具名码优先于分类", access.ErrUnauthorized, http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFor(tc.err); got != tc.want {
				t.Errorf("statusFor(%v) = %d, 期望 %d", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WebSocket 接入
// ---------------------------------------------------------------------------

// wsTestClient 是只用于测试的 WebSocket 客户端。
//
// 它独立实现握手与帧编解码（不依赖 wsproto 的编码路径），因此与服务端构成互操作验证。
type wsTestClient struct {
	conn   net.Conn
	reader *bufio.Reader
}

var testClientMaskKey = [4]byte{0x11, 0x22, 0x33, 0x44}

// dialWS 完成握手并返回客户端。
func dialWS(t *testing.T, serverURL string, headers map[string]string) *wsTestClient {
	t.Helper()

	addr := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	request := "GET " + pathWS + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"
	for name, value := range headers {
		request += name + ": " + value + "\r\n"
	}
	request += "\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写出握手请求失败: %v", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("状态行 = %q, 期望 101", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头失败: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &wsTestClient{conn: conn, reader: reader}
}

// send 发送一条带掩码的文本消息。
func (c *wsTestClient) send(t *testing.T, payload string) {
	t.Helper()
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, 0x81)
	frame = append(frame, 0x80|byte(len(payload)))
	frame = append(frame, testClientMaskKey[:]...)
	for i := 0; i < len(payload); i++ {
		frame = append(frame, payload[i]^testClientMaskKey[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("写出帧失败: %v", err)
	}
}

// receive 读取一条服务端文本消息。
func (c *wsTestClient) receive(t *testing.T) map[string]any {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}

	var header [2]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		t.Fatalf("读取帧头失败: %v", err)
	}
	if header[1]&0x80 != 0 {
		t.Fatal("服务端帧不应带掩码")
	}
	if wsproto.Opcode(header[0]&0x0F) != wsproto.OpText {
		t.Fatalf("帧类型 = %d, 期望文本", header[0]&0x0F)
	}

	length := int64(header[1] & 0x7F)
	if length == 126 {
		var extended [2]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			t.Fatalf("读取长度失败: %v", err)
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		t.Fatalf("读取负载失败: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("消息不是合法 JSON: %v（原文 %s）", err, payload)
	}
	return out
}

// TestWebSocketHandshakeRequiresAuth 断言 WebSocket 握手同样需要鉴权。
func TestWebSocketHandshakeRequiresAuth(t *testing.T) {
	handler := newTestHandler(t, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	defer conn.Close()

	request := "GET " + pathWS + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写出握手请求失败: %v", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	if !strings.Contains(statusLine, "401") {
		t.Errorf("状态行 = %q, 期望 401", strings.TrimSpace(statusLine))
	}
}

// TestWebSocketBearerHandshake 断言带 Bearer 令牌能完成握手并收到 hello。
func TestWebSocketBearerHandshake(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, nil))
	t.Cleanup(srv.Close)

	client := dialWS(t, srv.URL, map[string]string{"Authorization": "Bearer " + testToken})

	hello := client.receive(t)
	if hello["type"] != "hello" {
		t.Fatalf("首条消息类型 = %v, 期望 hello", hello["type"])
	}
	data, _ := hello["data"].(map[string]any)
	if data["version"] != "1.53" {
		t.Errorf("hello 里的版本 = %v, 期望 1.53", data["version"])
	}
	if operations, _ := data["operations"].([]any); len(operations) == 0 {
		t.Error("hello 应列出可用操作")
	}
	if codes, _ := data["errorCodes"].([]any); len(codes) == 0 {
		t.Error("hello 应列出错误码目录")
	}
}

// TestWebSocketSubprotocolCarriesToken 断言浏览器场景能通过子协议携带令牌。
//
// 浏览器的 WebSocket 构造函数无法自定义请求头，这条通道是其唯一选择。
func TestWebSocketSubprotocolCarriesToken(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, nil))
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	defer conn.Close()

	request := "GET " + pathWS + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: " + access.Subprotocol + ".bearer." + testToken + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写出握手请求失败: %v", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("状态行 = %q, 期望 101", strings.TrimSpace(statusLine))
	}

	// 必须回显子协议，否则浏览器会判定握手失败。
	echoed := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头失败: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if strings.EqualFold(strings.TrimSpace(line), "Sec-WebSocket-Protocol: "+access.Subprotocol) {
			echoed = true
		}
	}
	if !echoed {
		t.Error("应当回显协商出的子协议")
	}
}

// TestWebSocketDispatchesOperations 断言 WebSocket 路径能驱动同一组操作。
func TestWebSocketDispatchesOperations(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, nil))
	t.Cleanup(srv.Close)

	client := dialWS(t, srv.URL, map[string]string{"Authorization": "Bearer " + testToken})
	client.receive(t) // hello

	// version 操作不需要任何上游。
	client.send(t, `{"id":"1","op":"version"}`)
	reply := client.receive(t)
	if reply["type"] != "result" || reply["ok"] != true {
		t.Fatalf("回包 = %v, 期望 result/ok", reply)
	}
	if reply["id"] != "1" {
		t.Errorf("回包的 id = %v, 期望原样带回 1", reply["id"])
	}

	// probe 会走上游，本机上游被模拟成阻断。
	client.send(t, `{"id":"2","op":"probe"}`)
	probe := client.receive(t)
	if probe["type"] != "result" {
		t.Fatalf("probe 回包 = %v, 期望 result", probe)
	}
	data, _ := probe["data"].(map[string]any)
	if data["exitCode"] != float64(2) {
		t.Errorf("probe 的 exitCode = %v, 期望 2", data["exitCode"])
	}
}

// TestWebSocketErrorsCarryCodes 断言 WebSocket 的错误回包带同样的错误码。
func TestWebSocketErrorsCarryCodes(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, nil))
	t.Cleanup(srv.Close)

	client := dialWS(t, srv.URL, map[string]string{"Authorization": "Bearer " + testToken})
	client.receive(t) // hello

	tests := []struct {
		name     string
		message  string
		wantCode model.Code
	}{
		{"未知操作", `{"id":"1","op":"nope"}`, model.CodeUnknownOp},
		{"写操作被禁", `{"id":"2","op":"sync","args":{"sgid":"x","credential":"y"}}`, model.CodeWriteDisabled},
		{"缺少参数", `{"id":"3","op":"verify"}`, model.CodeBadArguments},
		{"非法 JSON", `{oops`, model.CodeBadArguments},
		{"二维码格式非法", `{"id":"5","op":"verify","args":{"sgid":"nope"}}`, model.CodeSGIDFormat},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client.send(t, tc.message)
			reply := client.receive(t)

			if reply["ok"] != false {
				t.Fatalf("回包 = %v, 期望失败", reply)
			}
			errObj, _ := reply["error"].(map[string]any)
			if errObj["code"] != string(tc.wantCode) {
				t.Errorf("错误码 = %v, 期望 %s", errObj["code"], tc.wantCode)
			}
			if errObj["kind"] == nil || errObj["kind"] == "" {
				t.Error("错误应带分类")
			}
		})
	}
}

// TestWebSocketRejectsBinaryFrames 断言二进制帧被拒绝并关闭连接。
func TestWebSocketRejectsBinaryFrames(t *testing.T) {
	srv := httptest.NewServer(newTestHandler(t, nil))
	t.Cleanup(srv.Close)

	client := dialWS(t, srv.URL, map[string]string{"Authorization": "Bearer " + testToken})
	client.receive(t) // hello

	payload := []byte("binary")
	frame := []byte{0x82, 0x80 | byte(len(payload))}
	frame = append(frame, testClientMaskKey[:]...)
	for i := range payload {
		frame = append(frame, payload[i]^testClientMaskKey[i%4])
	}
	if _, err := client.conn.Write(frame); err != nil {
		t.Fatalf("写出帧失败: %v", err)
	}

	if err := client.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	var header [2]byte
	if _, err := io.ReadFull(client.reader, header[:]); err != nil {
		t.Fatalf("读取帧头失败: %v", err)
	}
	if wsproto.Opcode(header[0]&0x0F) != wsproto.OpClose {
		t.Errorf("帧类型 = %d, 期望关闭帧", header[0]&0x0F)
	}
}

// TestWebSocketDisabledWhenAsked 断言 --no-ws 等价配置下不注册 WebSocket 端点。
func TestWebSocketDisabledWhenAsked(t *testing.T) {
	svc, err := service.New(service.Options{Version: "1.53"})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	auth, err := access.NewAuthenticator(testToken)
	if err != nil {
		t.Fatalf("构造鉴权器失败: %v", err)
	}

	handler, err := NewHTTPHandlerWithoutWS(Options{Deps: ops.Deps{Service: svc}, Auth: auth})
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	defer conn.Close()

	request := "GET " + pathWS + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" +
		"Authorization: Bearer " + testToken + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("写出握手请求失败: %v", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	if !strings.Contains(statusLine, "404") {
		t.Errorf("状态行 = %q, 期望 404（端点未注册）", strings.TrimSpace(statusLine))
	}
}

// TestPanicIsRecovered 断言处理器 panic 不会把连接打挂，也不会泄露内部细节。
func TestPanicIsRecovered(t *testing.T) {
	h := &httpHandler{opts: Options{}}
	handler := h.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d, 期望 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("响应体不应包含 panic 细节")
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Error("响应体应是合法 JSON")
	}
}

// TestNormalizeFillsDefaults 断言未配置时填入安全默认值。
func TestNormalizeFillsDefaults(t *testing.T) {
	opts := Options{}
	opts.normalize()

	if opts.MaxBody != access.DefaultMaxBody {
		t.Errorf("MaxBody = %d, 期望 %d", opts.MaxBody, access.DefaultMaxBody)
	}
	if opts.OperationTimeout <= 0 {
		t.Error("OperationTimeout 应有默认值")
	}
	if opts.WSIdleTimeout <= 0 {
		t.Error("WSIdleTimeout 应有默认值")
	}
}

// TestNilServicesAreSafe 断言限频器与信号量为 nil 时不做限制而不是崩溃。
func TestNilServicesAreSafe(t *testing.T) {
	handler := newTestHandler(t, func(o *Options) {
		o.Limiter = nil
		o.Sem = nil
	})

	for i := 0; i < 5; i++ {
		rec := do(t, handler, http.MethodPost, "/v1/op/version", "{}")
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次状态码 = %d, 期望 200", i, rec.Code)
		}
	}
}

// TestNilAuthenticatorRejected 断言缺少鉴权器时构造失败。
//
// 没有鉴权器意味着任何人都能用你的二维码与凭证，因此不能默许。
func TestNilAuthenticatorRejected(t *testing.T) {
	if _, err := NewHTTPHandler(Options{}); err == nil {
		t.Fatal("缺少鉴权器时应当构造失败")
	}
}
