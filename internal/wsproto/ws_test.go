package wsproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// rfcMaskKey 是 RFC 6455 §5.7 掩码样例里使用的密钥。
var rfcMaskKey = [4]byte{0x37, 0xfa, 0x21, 0x3d}

// ---------------------------------------------------------------------------
// 测试用客户端
//
// 本包只实现服务端方向（读带掩码、写不带掩码），所以测试需要自己写一个客户端。
// 这是有意的：客户端与服务端不共用编解码代码，测试因此成为两个独立实现之间的
// 互操作验证，而不是用同一份可能出错的逻辑自我印证。
// ---------------------------------------------------------------------------

// testClient 是只用于测试的最小 WebSocket 客户端。
type testClient struct {
	br      *bufio.Reader
	w       io.Writer
	maskKey [4]byte
}

// newTestClient 构造测试客户端。
func newTestClient(r io.Reader, w io.Writer) *testClient {
	return &testClient{br: bufio.NewReader(r), w: w, maskKey: rfcMaskKey}
}

// WriteMessage 发送一条带掩码的客户端消息。
func (c *testClient) WriteMessage(opcode Opcode, payload []byte) error {
	_, err := c.w.Write(clientFrame(true, opcode, payload, c.maskKey))
	return err
}

// ReadMessage 读取一条服务端消息，跳过心跳。
func (c *testClient) ReadMessage() (Opcode, []byte, error) {
	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if !fin {
			return 0, nil, errors.New("测试客户端不支持分片")
		}
		switch opcode {
		case OpText, OpBinary:
			return opcode, payload, nil
		case OpPing, OpPong:
			continue
		case OpClose:
			return OpClose, payload, nil
		default:
			return 0, nil, errors.New("测试客户端收到未知帧类型")
		}
	}
}

// readFrame 读取一个服务端帧（按 RFC 必须不带掩码）。
func (c *testClient) readFrame() (bool, Opcode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return false, 0, nil, err
	}
	if header[1]&0x80 != 0 {
		return false, 0, nil, errors.New("服务端帧不应带掩码")
	}

	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.br, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.br, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended[:]))
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	return header[0]&0x80 != 0, Opcode(header[0] & 0x0F), payload, nil
}

// clientFrame 按 RFC 6455 §5.2 的布局构造一个带掩码的客户端帧。
func clientFrame(fin bool, opcode Opcode, payload []byte, key [4]byte) []byte {
	frame := make([]byte, 0, len(payload)+14)
	var first byte
	if fin {
		first |= 0x80
	}
	frame = append(frame, first|byte(opcode)&0x0F)

	switch {
	case len(payload) < 126:
		frame = append(frame, 0x80|byte(len(payload)))
	case len(payload) <= 0xFFFF:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}
	frame = append(frame, key[:]...)

	masked := make([]byte, len(payload))
	copy(masked, payload)
	applyMask(masked, key)
	return append(frame, masked...)
}

// ---------------------------------------------------------------------------
// 握手
// ---------------------------------------------------------------------------

// TestAcceptKeyOfficialVector 用 RFC 6455 §1.3 的官方样例断言握手摘要算法。
func TestAcceptKeyOfficialVector(t *testing.T) {
	tests := []struct {
		name      string
		clientKey string
		want      string
	}{
		{"RFC 官方样例", "dGhlIHNhbXBsZSBub25jZQ==", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="},
		{"另一个密钥也必须是确定值", "x3JJHMbDL1EzLkh9GBhXDw==", "HSmrc0sMlYUkAGmm5OPpG2HaGWk="},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AcceptKey(tc.clientKey); got != tc.want {
				t.Errorf("AcceptKey(%q) = %q, 期望 %q", tc.clientKey, got, tc.want)
			}
		})
	}
}

// newUpgradeRequest 构造一个合法的升级请求。
func newUpgradeRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	return req
}

// TestUpgradeRejectsBadRequests 断言非法升级请求被拒绝。
func TestUpgradeRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"缺少 Upgrade 头", map[string]string{"Connection": "Upgrade", "Sec-WebSocket-Key": "k", "Sec-WebSocket-Version": "13"}},
		{"缺少 Connection 头", map[string]string{"Upgrade": "websocket", "Sec-WebSocket-Key": "k", "Sec-WebSocket-Version": "13"}},
		{"版本不是 13", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Key": "k", "Sec-WebSocket-Version": "8"}},
		{"缺少密钥", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Version": "13"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if _, err := Upgrade(httptest.NewRecorder(), req, ""); err == nil {
				t.Fatal("非法升级请求应当被拒绝")
			}
		})
	}
}

// TestUpgradeOnPlainHTTPWriterFails 断言不支持接管的 ResponseWriter 得到明确错误。
//
// httptest.ResponseRecorder 不能接管，正好覆盖这条路径：HTTP/2 下同样不可用。
func TestUpgradeOnPlainHTTPWriterFails(t *testing.T) {
	_, err := Upgrade(httptest.NewRecorder(), newUpgradeRequest(), "")
	if err == nil {
		t.Fatal("不可接管的 ResponseWriter 应当报错")
	}
	if !errors.Is(err, ErrNotWebSocket) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrNotWebSocket)
	}
	if !strings.Contains(err.Error(), "HTTP/1.1") {
		t.Errorf("错误信息应提示需要 HTTP/1.1: %v", err)
	}
}

// TestUpgradeHandshakeEndToEnd 在真实 TCP 上完成握手并双向收发。
func TestUpgradeHandshakeEndToEnd(t *testing.T) {
	const clientKey = "dGhlIHNhbXBsZSBub25jZQ=="

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrade(w, r, "mai-arcade.v1")
		if err != nil {
			t.Errorf("Upgrade 报错: %v", err)
			return
		}
		defer conn.Close()

		opcode, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage 报错: %v", err)
			return
		}
		if opcode != OpText {
			t.Errorf("opcode = %s, 期望 text", opcode)
		}
		if err := conn.WriteText(append([]byte("echo:"), payload...)); err != nil {
			t.Errorf("WriteText 报错: %v", err)
		}
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	addr := strings.TrimPrefix(server.URL, "http://")
	netConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	defer netConn.Close()

	handshake := "GET /v1/ws HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + clientKey + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := netConn.Write([]byte(handshake)); err != nil {
		t.Fatalf("写出握手请求失败: %v", err)
	}

	reader := bufio.NewReader(netConn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("状态行 = %q, 期望 101", strings.TrimSpace(statusLine))
	}

	headers := map[string]string{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取响应头失败: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}

	if got, want := headers["sec-websocket-accept"], AcceptKey(clientKey); got != want {
		t.Errorf("Sec-WebSocket-Accept = %q, 期望 %q", got, want)
	}
	if got := headers["sec-websocket-protocol"]; got != "mai-arcade.v1" {
		t.Errorf("子协议 = %q, 期望 mai-arcade.v1", got)
	}

	client := newTestClient(reader, netConn)
	if err := client.WriteMessage(OpText, []byte("ping")); err != nil {
		t.Fatalf("客户端写失败: %v", err)
	}
	opcode, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("客户端读失败: %v", err)
	}
	if opcode != OpText || string(payload) != "echo:ping" {
		t.Errorf("回声 = %s %q, 期望 text echo:ping", opcode, payload)
	}
}

// ---------------------------------------------------------------------------
// 帧编码（服务端方向）
// ---------------------------------------------------------------------------

// TestEncodeTextFrameOfficialVector 断言服务端文本帧与 RFC 样例逐字节一致。
//
// RFC 6455 §5.7 例一：不带掩码的 "Hello" 是 81 05 48 65 6c 6c 6f。
func TestEncodeTextFrameOfficialVector(t *testing.T) {
	pipe := newBufferPipe(nil)
	conn := newConn(pipe, nil)

	if err := conn.WriteText([]byte("Hello")); err != nil {
		t.Fatalf("WriteText 报错: %v", err)
	}

	assertHex(t, pipe.outgoing.Bytes(), "810548656c6c6f")
}

// TestEncodeExtendedLengthOfficialVectors 断言 16 位与 64 位长度字段的编码。
//
// RFC 6455 §5.7 例三与例四：256 字节用 126 + 16 位长度，65536 字节用 127 + 64 位长度。
func TestEncodeExtendedLengthOfficialVectors(t *testing.T) {
	t.Run("256 字节用 16 位长度", func(t *testing.T) {
		payload := make([]byte, 256)
		for i := range payload {
			payload[i] = byte(i)
		}
		pipe := newBufferPipe(nil)
		conn := newConn(pipe, nil)
		if err := conn.WriteMessage(OpBinary, payload); err != nil {
			t.Fatalf("WriteMessage 报错: %v", err)
		}

		got := pipe.outgoing.Bytes()
		assertHex(t, got[:4], "827e0100")
		if len(got) != 4+256 {
			t.Errorf("帧总长 = %d, 期望 %d", len(got), 4+256)
		}
		if !bytes.Equal(got[4:], payload) {
			t.Error("负载被改动了")
		}
	})

	t.Run("65536 字节用 64 位长度", func(t *testing.T) {
		payload := bytes.Repeat([]byte{0xAB}, 65536)
		pipe := newBufferPipe(nil)
		conn := newConn(pipe, nil)
		conn.SetMaxMessage(1 << 20)
		if err := conn.WriteMessage(OpBinary, payload); err != nil {
			t.Fatalf("WriteMessage 报错: %v", err)
		}

		got := pipe.outgoing.Bytes()
		assertHex(t, got[:10], "827f0000000000010000")
		if len(got) != 10+65536 {
			t.Errorf("帧总长 = %d, 期望 %d", len(got), 10+65536)
		}
	})
}

// TestEncodeCloseFrameCarriesCodeAndReason 断言关闭帧带上状态码与原因。
func TestEncodeCloseFrameCarriesCodeAndReason(t *testing.T) {
	pipe := newBufferPipe(nil)
	conn := newConn(pipe, nil)

	if err := conn.WriteClose(ClosePolicyViolation, "no"); err != nil {
		t.Fatalf("WriteClose 报错: %v", err)
	}

	got := pipe.outgoing.Bytes()
	if Opcode(got[0]&0x0F) != OpClose {
		t.Fatalf("opcode = %s, 期望 close", Opcode(got[0]&0x0F))
	}
	if code := binary.BigEndian.Uint16(got[2:4]); code != ClosePolicyViolation {
		t.Errorf("关闭码 = %d, 期望 %d", code, ClosePolicyViolation)
	}
	if reason := string(got[4:]); reason != "no" {
		t.Errorf("关闭原因 = %q, 期望 no", reason)
	}
}

// ---------------------------------------------------------------------------
// 帧解码（客户端方向）
// ---------------------------------------------------------------------------

// TestClientFrameLayoutMatchesRFC 断言测试客户端的帧构造与 RFC 样例一致。
//
// 测试客户端是独立的第二实现，它本身也要先被官方样例锚定，后续用例才可信。
func TestClientFrameLayoutMatchesRFC(t *testing.T) {
	// RFC 6455 §5.7 例二：掩码密钥 37 fa 21 3d 时 "Hello" 是
	// 81 85 37 fa 21 3d 7f 9f 4d 51 58。
	assertHex(t, clientFrame(true, OpText, []byte("Hello"), rfcMaskKey), "818537fa213d7f9f4d5158")
}

// TestDecodeMaskedTextFrame 断言能解析客户端发来的带掩码文本帧。
func TestDecodeMaskedTextFrame(t *testing.T) {
	conn := newConn(newBufferPipe(clientFrame(true, OpText, []byte("Hello"), rfcMaskKey)), nil)

	opcode, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage 报错: %v", err)
	}
	if opcode != OpText || string(payload) != "Hello" {
		t.Errorf("得到 %s %q, 期望 text Hello", opcode, payload)
	}
}

// TestDecodeFragmentedMessage 断言分片消息被拼装成一条。
//
// RFC 6455 §5.7 例五：FIN=0 的 "Hel" 后接 FIN=1 的续帧 "lo" 等于 "Hello"。
func TestDecodeFragmentedMessage(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(clientFrame(false, OpText, []byte("Hel"), rfcMaskKey))
	stream.Write(clientFrame(true, OpContinuation, []byte("lo"), rfcMaskKey))

	conn := newConn(newBufferPipe(stream.Bytes()), nil)
	opcode, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage 报错: %v", err)
	}
	if opcode != OpText || string(payload) != "Hello" {
		t.Errorf("得到 %s %q, 期望 text Hello", opcode, payload)
	}
}

// TestDecodeRejectsProtocolViolations 断言违反协议的帧会被拒绝。
func TestDecodeRejectsProtocolViolations(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			// 客户端帧必须带掩码（RFC 6455 §5.1）。
			name: "未带掩码的客户端帧",
			raw:  []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'},
		},
		{
			// RSV 位非零但未协商扩展。
			name: "RSV 位非零",
			raw:  clientFrameWithFirstByte(0xC1, []byte("Hello"), rfcMaskKey),
		},
		{
			// 控制帧负载不能超过 125（RFC 6455 §5.5）。
			name: "控制帧负载过大",
			raw:  append([]byte{0x89, 0xFE, 0x00, 0x80}, bytes.Repeat([]byte{0}, 128)...),
		},
		{
			// 64 位长度的最高位必须为 0（RFC 6455 §5.2）。
			name: "长度最高位非零",
			raw:  append([]byte{0x82, 0xFF}, append(bytes.Repeat([]byte{0}, 7), 0x01)...),
		},
		{
			// 上一条分片消息未结束时又来了一条新的数据帧。
			name: "分片未结束就开新消息",
			raw: append(
				clientFrame(false, OpText, []byte("He"), rfcMaskKey),
				clientFrame(true, OpText, []byte("llo"), rfcMaskKey)...),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newConn(newBufferPipe(tc.raw), nil)
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Fatal("违反协议的帧应当被拒绝")
			}
		})
	}
}

// clientFrameWithFirstByte 构造一个首字节被改写的客户端帧，用于制造协议违规。
func clientFrameWithFirstByte(first byte, payload []byte, key [4]byte) []byte {
	frame := clientFrame(true, OpText, payload, key)
	frame[0] = first
	return frame
}

// TestDecodeRejectsOrphanContinuation 断言没有起始帧的续帧被拒绝。
func TestDecodeRejectsOrphanContinuation(t *testing.T) {
	conn := newConn(newBufferPipe(clientFrame(true, OpContinuation, []byte("lo"), rfcMaskKey)), nil)
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("孤立续帧应当被拒绝")
	}
}

// TestDecodeRejectsInvalidUTF8Text 断言非法 UTF-8 的文本帧被拒绝（RFC 要求 1007）。
func TestDecodeRejectsInvalidUTF8Text(t *testing.T) {
	conn := newConn(newBufferPipe(clientFrame(true, OpText, []byte{0xFF, 0xFE}, rfcMaskKey)), nil)
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("非法 UTF-8 应当被拒绝")
	}
}

// TestPingIsAnsweredWithPong 断言 ping 自动被回以 pong，且负载原样带回。
//
// 心跳响应是协议要求，不该交给调用方实现，否则很容易被漏掉。
func TestPingIsAnsweredWithPong(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(clientFrame(true, OpPing, []byte("hi"), rfcMaskKey))
	stream.Write(clientFrame(true, OpText, []byte("ok"), rfcMaskKey))

	pipe := newBufferPipe(stream.Bytes())
	conn := newConn(pipe, nil)

	opcode, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage 报错: %v", err)
	}
	if opcode != OpText || string(payload) != "ok" {
		t.Fatalf("得到 %s %q, 期望跳过 ping 后拿到文本 ok", opcode, payload)
	}

	// 服务端回的 pong 不带掩码，负载可直接在字节流里看到。
	pong := pipe.outgoing.Bytes()
	if len(pong) < 2 || Opcode(pong[0]&0x0F) != OpPong {
		t.Fatalf("应当先回一个 pong，实际写出 %s", hex.EncodeToString(pong))
	}
	if !bytes.Contains(pong, []byte("hi")) {
		t.Error("pong 应原样带回 ping 的负载")
	}
}

// TestCloseFrameReturnsCloseErrorAndReplies 断言收到关闭帧时回一个关闭帧并返回 CloseError。
func TestCloseFrameReturnsCloseErrorAndReplies(t *testing.T) {
	payload := append([]byte{0x03, 0xE8}, []byte("bye")...) // 1000 + "bye"
	pipe := newBufferPipe(clientFrame(true, OpClose, payload, rfcMaskKey))
	conn := newConn(pipe, nil)

	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("收到关闭帧应当返回错误")
	}

	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("错误类型 = %T, 期望 *CloseError", err)
	}
	if closeErr.Code != CloseNormal {
		t.Errorf("关闭码 = %d, 期望 %d", closeErr.Code, CloseNormal)
	}
	if closeErr.Reason != "bye" {
		t.Errorf("关闭原因 = %q, 期望 bye", closeErr.Reason)
	}
	// 关闭错误也应能被当作流结束处理，便于统一收尾。
	if !errors.Is(err, io.EOF) {
		t.Error("CloseError 应当能被 errors.Is(err, io.EOF) 匹配")
	}

	reply := pipe.outgoing.Bytes()
	if len(reply) < 2 || Opcode(reply[0]&0x0F) != OpClose {
		t.Fatalf("应当回一个关闭帧，实际写出 %s", hex.EncodeToString(reply))
	}
}

// TestMessageTooBig 断言超过上限的消息被拒绝。
func TestMessageTooBig(t *testing.T) {
	conn := newConn(newBufferPipe(clientFrame(true, OpText, bytes.Repeat([]byte("A"), 2048), rfcMaskKey)), nil)
	conn.SetMaxMessage(1024)

	if _, _, err := conn.ReadMessage(); !errors.Is(err, ErrMessageTooBig) {
		t.Fatalf("错误 = %v, 期望 %v", err, ErrMessageTooBig)
	}
}

// TestConcurrentWritesDoNotInterleave 断言并发写不会交错破坏帧结构。
//
// -race 会同时检查这里是否存在数据竞争。
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	pipe := newBufferPipe(nil)
	conn := newConn(pipe, nil)

	const writers, messages = 8, 20
	payload := []byte(`{"ok":true}`)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < messages; j++ {
				if err := conn.WriteText(payload); err != nil {
					t.Errorf("并发写报错: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// 用独立实现的测试客户端读回，帧数与内容都必须正确。
	client := newTestClient(bytes.NewReader(pipe.outgoing.Bytes()), io.Discard)
	count := 0
	for {
		_, got, err := client.ReadMessage()
		if err != nil {
			break
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("第 %d 条消息内容被破坏: %q", count, got)
		}
		count++
	}
	if count != writers*messages {
		t.Errorf("帧数 = %d, 期望 %d（说明写出被交错破坏）", count, writers*messages)
	}
}

// TestHeaderHasToken 断言头值里的 token 匹配大小写不敏感且支持逗号列表。
func TestHeaderHasToken(t *testing.T) {
	header := http.Header{}
	header.Set("Connection", "keep-alive, Upgrade")
	header.Set("Upgrade", "WebSocket")

	if !headerHasToken(header, "Connection", "upgrade") {
		t.Error("逗号列表里的 token 应被识别")
	}
	if !headerHasToken(header, "Upgrade", "websocket") {
		t.Error("大小写不应影响匹配")
	}
	if headerHasToken(header, "Upgrade", "h2c") {
		t.Error("不应匹配不存在的 token")
	}
}

// bufferPipe 是一个可完全控制字节流的双向管道，用来确定性地喂帧与断言写出。
type bufferPipe struct {
	incoming *bytes.Buffer
	outgoing *bytes.Buffer
}

func newBufferPipe(incoming []byte) *bufferPipe {
	return &bufferPipe{incoming: bytes.NewBuffer(incoming), outgoing: &bytes.Buffer{}}
}

func (p *bufferPipe) Read(b []byte) (int, error)  { return p.incoming.Read(b) }
func (p *bufferPipe) Write(b []byte) (int, error) { return p.outgoing.Write(b) }
func (p *bufferPipe) Close() error                { return nil }

// assertHex 比对字节切片与十六进制串。
func assertHex(t *testing.T, got []byte, wantHex string) {
	t.Helper()
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("十六进制串 %q 无法解析: %v", wantHex, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("字节不符\n实际 %s\n期望 %s", hex.EncodeToString(got), wantHex)
	}
}
