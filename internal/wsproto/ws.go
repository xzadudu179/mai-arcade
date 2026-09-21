// Package wsproto 是零依赖的 WebSocket（RFC 6455）服务端实现：握手加帧编解码。
//
// 为什么自己实现而不用第三方库：本项目的核心决策之一是零第三方依赖
// （单静态二进制、可随时 scp 到别的网络环境、供应链干净），而 Go 标准库没有 WebSocket。
// 服务端只需要握手与帧这两件事，实现量可控，且能用 RFC 自带的官方样例断言。
//
// 范围与限制：不做扩展协商（RSV 位必须为 0）、不做压缩、只实现服务端方向
// （收到的客户端帧必须带掩码，发出的帧不带掩码）。
package wsproto

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MagicGUID 是握手摘要计算用的固定串（RFC 6455 §4.2.2）。
const MagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// AcceptKey 由客户端密钥算出 Sec-WebSocket-Accept。
//
// RFC 6455 §1.3 给出的官方样例：密钥 dGhlIHNhbXBsZSBub25jZQ== 对应
// s3pPLMBiTxaQ9kYGzzhZRbK+xOo=。
func AcceptKey(clientKey string) string {
	sum := sha1.Sum([]byte(clientKey + MagicGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Opcode 是帧类型。
type Opcode byte

// RFC 6455 §5.2 定义的帧类型。当前只用到其中的文本、关闭与心跳。
const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA

	// maxControlPayload 是控制帧负载上限（RFC 6455 §5.5）。
	maxControlPayload = 125
)

// String 返回帧类型的可读名。
func (o Opcode) String() string {
	switch o {
	case OpContinuation:
		return "continuation"
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	default:
		return fmt.Sprintf("opcode(0x%x)", byte(o))
	}
}

// 关闭码（RFC 6455 §7.4.1）。
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	CloseInvalidPayload  = 1007
	ClosePolicyViolation = 1008
	CloseMessageTooBig   = 1009
	CloseInternalError   = 1011
)

// 协议层哨兵错误。
var (
	// ErrNotWebSocket 表示请求不是一次合法的 WebSocket 升级。
	ErrNotWebSocket = errors.New("wsproto: 不是合法的 WebSocket 升级请求")

	// ErrProtocol 表示对端违反了协议；应带 1002 关闭连接。
	ErrProtocol = errors.New("wsproto: 对端违反协议")

	// ErrMessageTooBig 表示消息超过上限；应带 1009 关闭连接。
	ErrMessageTooBig = errors.New("wsproto: 消息超过上限")
)

// CloseError 表示对端主动关闭，并带回关闭码与原因。
type CloseError struct {
	Code   int
	Reason string
}

// Error 实现 error。
func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("wsproto: 对端关闭连接（码 %d）", e.Code)
	}
	return fmt.Sprintf("wsproto: 对端关闭连接（码 %d：%s）", e.Code, e.Reason)
}

// Is 让 errors.Is(err, io.EOF) 也能匹配关闭错误，便于统一处理收尾。
func (e *CloseError) Is(target error) bool {
	return target == io.EOF || target == io.ErrUnexpectedEOF
}

// DefaultMaxMessage 是单条消息的默认上限。
const DefaultMaxMessage = 1 << 20

// Conn 是一条 WebSocket 连接。
type Conn struct {
	rw io.ReadWriteCloser
	br *bufio.Reader
	bw *bufio.Writer

	// maskOutgoing 为真时发出的帧带掩码。服务端必须为假；测试里当客户端用时置真。
	maskOutgoing bool

	maxMessage int64

	// maskKey 只在测试方向（maskOutgoing）使用：注入固定值可对齐 RFC 样例，
	// 否则用随机掩码。服务端方向永不掩码。
	maskKey *[4]byte

	writeMu sync.Mutex
	closed  bool
}

// Upgrade 校验升级请求、完成握手，并返回连接。
//
// subprotocol 非空时会回显 Sec-WebSocket-Protocol，浏览器场景用它携带凭证。
func Upgrade(w http.ResponseWriter, r *http.Request, subprotocol string) (*Conn, error) {
	if err := validateUpgrade(r); err != nil {
		return nil, err
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("%w: 底层 ResponseWriter 不支持连接接管（HTTP/2 不支持 WebSocket，请用 HTTP/1.1）", ErrNotWebSocket)
	}
	raw, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, fmt.Errorf("接管连接: %w", err)
	}

	// 握手响应必须自己写：接管之后 net/http 不再参与。
	var handshake strings.Builder
	handshake.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	handshake.WriteString("Upgrade: websocket\r\n")
	handshake.WriteString("Connection: Upgrade\r\n")
	handshake.WriteString("Sec-WebSocket-Accept: " + AcceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n")
	if subprotocol != "" {
		handshake.WriteString("Sec-WebSocket-Protocol: " + subprotocol + "\r\n")
	}
	handshake.WriteString("\r\n")

	if _, err := raw.Write([]byte(handshake.String())); err != nil {
		raw.Close()
		return nil, fmt.Errorf("写出握手响应: %w", err)
	}

	return newConn(raw, buffered.Reader), nil
}

// validateUpgrade 校验 RFC 6455 §4.2.1 要求的请求头。
func validateUpgrade(r *http.Request) error {
	if !headerHasToken(r.Header, "Connection", "upgrade") {
		return fmt.Errorf("%w: Connection 头缺少 upgrade", ErrNotWebSocket)
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		return fmt.Errorf("%w: Upgrade 头不是 websocket", ErrNotWebSocket)
	}
	if version := r.Header.Get("Sec-WebSocket-Version"); version != "13" {
		return fmt.Errorf("%w: Sec-WebSocket-Version = %q，只支持 13", ErrNotWebSocket, version)
	}
	if r.Header.Get("Sec-WebSocket-Key") == "" {
		return fmt.Errorf("%w: 缺少 Sec-WebSocket-Key", ErrNotWebSocket)
	}
	return nil
}

// headerHasToken 在逗号分隔的头值里查找某个 token，大小写不敏感。
func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return true
			}
		}
	}
	return false
}

// newConn 把双向流包装成 WebSocket 连接。
func newConn(rw io.ReadWriteCloser, reader *bufio.Reader) *Conn {
	if reader == nil {
		reader = bufio.NewReader(rw)
	}
	return &Conn{
		rw:         rw,
		br:         reader,
		bw:         bufio.NewWriter(rw),
		maxMessage: DefaultMaxMessage,
	}
}

// SetMaxMessage 设置单条消息的大小上限。
func (c *Conn) SetMaxMessage(n int64) {
	if n > 0 {
		c.maxMessage = n
	}
}

// Remaining 返回接管连接时可能已被缓存的未读数据。
func (c *Conn) Remaining() *bufio.Reader { return c.br }

// ReadMessage 读取一条完整消息，内部处理分片、心跳与关闭。
//
// 收到 ping 会自动回 pong；收到 close 会回一个 close 并返回 *CloseError。
// 这两个行为都由协议要求，因此不该交给调用方处理。
func (c *Conn) ReadMessage() (Opcode, []byte, error) {
	var (
		payload    []byte
		messageOp  Opcode
		fragmented bool
	)

	for {
		fin, opcode, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		if opcode.isControl() {
			if !fin {
				return 0, nil, fmt.Errorf("%w: 控制帧不允许分片", ErrProtocol)
			}
			if err := c.handleControl(opcode, data); err != nil {
				return 0, nil, err
			}
			continue
		}

		switch opcode {
		case OpText, OpBinary:
			if fragmented {
				return 0, nil, fmt.Errorf("%w: 上一条分片消息尚未结束", ErrProtocol)
			}
			messageOp, payload, fragmented = opcode, data, !fin
		case OpContinuation:
			if !fragmented {
				return 0, nil, fmt.Errorf("%w: 收到没有起始帧的续帧", ErrProtocol)
			}
			payload = append(payload, data...)
			fragmented = !fin
		default:
			return 0, nil, fmt.Errorf("%w: 未知帧类型 %s", ErrProtocol, opcode)
		}

		if int64(len(payload)) > c.maxMessage {
			return 0, nil, ErrMessageTooBig
		}
		if fragmented {
			continue
		}

		// 文本消息必须是合法 UTF-8，否则按 RFC 用 1007 关闭。
		if messageOp == OpText && !utf8.Valid(payload) {
			return 0, nil, fmt.Errorf("%w: 文本帧不是合法 UTF-8", ErrProtocol)
		}
		return messageOp, payload, nil
	}
}

// handleControl 处理控制帧。
func (c *Conn) handleControl(opcode Opcode, data []byte) error {
	switch opcode {
	case OpPing:
		if err := c.writeFrame(OpPong, data, true); err != nil {
			return err
		}
		return nil
	case OpPong:
		return nil
	case OpClose:
		code, reason := parseClosePayload(data)
		// 回一个关闭帧完成握手；对端可能已经走了，因此忽略写失败。
		_ = c.WriteClose(CloseNormal, "")
		return &CloseError{Code: code, Reason: reason}
	default:
		return fmt.Errorf("%w: 未知控制帧 %s", ErrProtocol, opcode)
	}
}

// parseClosePayload 解析关闭帧里的状态码与原因。
func parseClosePayload(data []byte) (int, string) {
	if len(data) < 2 {
		return CloseNormal, ""
	}
	return int(binary.BigEndian.Uint16(data[:2])), string(data[2:])
}

// isControl 报告是否为控制帧。
func (o Opcode) isControl() bool { return o >= OpClose }

// WriteText 发送一条文本消息。
func (c *Conn) WriteText(payload []byte) error { return c.WriteMessage(OpText, payload) }

// WriteMessage 发送一条完整消息。
func (c *Conn) WriteMessage(opcode Opcode, payload []byte) error {
	return c.writeFrame(opcode, payload, true)
}

// WriteClose 发送关闭帧。
func (c *Conn) WriteClose(code int, reason string) error {
	payload := make([]byte, 0, 2+len(reason))
	payload = binary.BigEndian.AppendUint16(payload, uint16(code))
	payload = append(payload, reason...)
	if len(payload) > maxControlPayload {
		payload = payload[:maxControlPayload]
	}
	return c.writeFrame(OpClose, payload, true)
}

// Close 发送关闭帧并关闭底层连接。
func (c *Conn) Close() error {
	c.writeMu.Lock()
	alreadyClosed := c.closed
	c.closed = true
	c.writeMu.Unlock()

	if !alreadyClosed {
		_ = c.WriteClose(CloseNormal, "")
	}
	return c.rw.Close()
}

// readFrame 读取一个帧，完成长度解析与去掩码。
func (c *Conn) readFrame() (bool, Opcode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return false, 0, nil, err
	}

	fin := header[0]&0x80 != 0
	// RSV 位必须为 0：本实现不做扩展协商。
	if header[0]&0x70 != 0 {
		return false, 0, nil, fmt.Errorf("%w: RSV 位非零但未协商任何扩展", ErrProtocol)
	}
	opcode := Opcode(header[0] & 0x0F)

	masked := header[1]&0x80 != 0
	// 客户端发给服务端的帧必须带掩码（RFC 6455 §5.1）。
	if !masked {
		return false, 0, nil, fmt.Errorf("%w: 客户端帧未带掩码", ErrProtocol)
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
		value := binary.BigEndian.Uint64(extended[:])
		// 最高位必须为 0（RFC 6455 §5.2）。
		if value&(1<<63) != 0 {
			return false, 0, nil, fmt.Errorf("%w: 长度最高位非零", ErrProtocol)
		}
		length = int64(value)
	}

	if opcode.isControl() {
		if length > maxControlPayload {
			return false, 0, nil, fmt.Errorf("%w: 控制帧负载 %d 超过 %d", ErrProtocol, length, maxControlPayload)
		}
	}
	if length > c.maxMessage {
		return false, 0, nil, ErrMessageTooBig
	}

	var maskKey [4]byte
	if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
		return false, 0, nil, err
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	applyMask(payload, maskKey)

	return fin, opcode, payload, nil
}

// writeFrame 写出一个帧。
func (c *Conn) writeFrame(opcode Opcode, payload []byte, fin bool) error {
	// 并发的写会交错破坏帧结构，因此必须串行。
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	header := make([]byte, 0, 14)
	var first byte
	if fin {
		first |= 0x80
	}
	first |= byte(opcode) & 0x0F
	header = append(header, first)

	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xFFFF:
		header = append(header, 126)
		header = binary.BigEndian.AppendUint16(header, uint16(len(payload)))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(len(payload)))
	}

	// 服务端帧不带掩码，因此负载可以原样写出。
	if _, err := c.bw.Write(header); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// applyMask 就地异或掩码；掩码为空时不做任何事。
func applyMask(payload []byte, key [4]byte) {
	for i := range payload {
		payload[i] ^= key[i%4]
	}
}

// SetReadDeadline 设置读超时；底层连接不支持时返回错误。
func (c *Conn) SetReadDeadline(t time.Time) error {
	conn, ok := c.rw.(net.Conn)
	if !ok {
		return errors.New("wsproto: 底层连接不支持设置读超时")
	}
	return conn.SetReadDeadline(t)
}

// SetWriteDeadline 设置写超时；底层连接不支持时返回错误。
func (c *Conn) SetWriteDeadline(t time.Time) error {
	conn, ok := c.rw.(net.Conn)
	if !ok {
		return errors.New("wsproto: 底层连接不支持设置写超时")
	}
	return conn.SetWriteDeadline(t)
}
