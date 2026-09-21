package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/access"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/ops"
	"github.com/xzadudu179/maimai-arcade/internal/wsproto"
)

// WebSocket 协议约定（与 HTTP 版共用同一组操作）：
//
//	建立连接后服务端先发一条 hello 消息，说明版本与可用操作；
//	客户端随后发 {"id":"任意","op":"操作名","args":{...}}；
//	服务端回 {"type":"result"|"error","id":同一个 id,...}。
//
// 一条连接上的请求按到达顺序串行处理：机台侧本就要求同账号串行，
// 并发处理只会让请求互相等待，收益为零而复杂度翻倍。

// wsRequest 是客户端发来的一条请求。
type wsRequest struct {
	// ID 由客户端指定，回包会原样带回，用于把异步的响应与请求对上。
	ID string `json:"id"`

	// Op 是操作名，取值见 hello 消息或 /v1/ops。
	Op string `json:"op"`

	// Args 是操作参数。
	Args json.RawMessage `json:"args"`
}

// wsReply 是服务端发出的一条消息。
type wsReply struct {
	Type      string   `json:"type"`
	ID        string   `json:"id,omitempty"`
	OK        bool     `json:"ok"`
	Operation string   `json:"operation,omitempty"`
	Data      any      `json:"data,omitempty"`
	Error     *errBody `json:"error,omitempty"`
}

// subprotocolKey 让 protect 中间件把协商出的子协议传给 WebSocket 处理器。
type subprotocolKey struct{}

// upgradeWebSocket 完成握手并进入消息循环。
func (h *httpHandler) upgradeWebSocket(w http.ResponseWriter, r *http.Request) {
	subprotocol, _ := r.Context().Value(subprotocolKey{}).(string)

	conn, err := wsproto.Upgrade(w, r, subprotocol)
	if err != nil {
		// 握手失败发生在接管连接之前或接管失败时，因此仍能写出普通 HTTP 响应。
		h.fail(w, r, "ws", upgradeError(err), time.Now())
		return
	}
	defer conn.Close()

	h.serveWebSocket(conn)
}

// upgradeError 把 wsproto 的握手错误映射成参数错误（400），而不是内部错误。
func upgradeError(err error) error {
	if errors.Is(err, wsproto.ErrNotWebSocket) {
		return model.Errorf(model.KindParam, model.CodeBadArguments, "", "WebSocket 升级请求不合法", "", err)
	}
	return model.Errorf(model.KindNetwork, model.CodeNetwork, "", "无法建立 WebSocket 连接", "", err)
}

// serveWebSocket 处理一条连接上的全部消息，直到对端关闭或空闲超时。
func (h *httpHandler) serveWebSocket(conn *wsproto.Conn) {
	conn.SetMaxMessage(h.opts.MaxBody)

	if err := h.writeWS(conn, wsReply{
		Type: "hello",
		OK:   true,
		Data: map[string]any{
			"version":      h.opts.Deps.Service.Version(),
			"writeEnabled": h.opts.Deps.AllowWrite,
			"operations":   ops.Describe(),
			"errorCodes":   model.Errors(),
			"encoding":     "文本帧承载 JSON；字段与 HTTP 版一致",
		},
	}); err != nil {
		h.logWS("发送 hello 失败", err)
		return
	}

	for {
		if err := conn.SetReadDeadline(time.Now().Add(h.opts.WSIdleTimeout)); err != nil {
			h.logWS("设置读超时失败", err)
			return
		}
		opcode, payload, err := conn.ReadMessage()
		if err != nil {
			// 对端正常关闭与超时都属于会话结束，不需要当成故障刷日志。
			if !isExpectedWSClose(err) {
				h.logWS("读取消息结束", err)
			}
			return
		}
		if opcode != wsproto.OpText {
			// 本协议只接受文本帧；二进制帧无法承载约定的 JSON。
			if err := conn.WriteClose(wsproto.CloseUnsupportedData, "只接受文本帧"); err != nil {
				return
			}
			continue
		}

		reply, fatal := h.handleWSMessage(payload)
		if err := h.writeWS(conn, reply); err != nil {
			h.logWS("写出响应失败", err)
			return
		}
		if fatal {
			_ = conn.WriteClose(wsproto.ClosePolicyViolation, "参数过大或协议错误")
			return
		}
	}
}

// handleWSMessage 处理一条请求，返回回包与是否应当关闭连接。
func (h *httpHandler) handleWSMessage(payload []byte) (wsReply, bool) {
	var request wsRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return errorReply("", "", model.Errorf(model.KindParam, model.CodeBadArguments,
			"", "消息不是合法 JSON", "请求体应为 {\"id\":\"1\",\"op\":\"verify\",\"args\":{...}}", err)), false
	}
	// 限频在握手时查过一次，这里对每条消息再查一次：
	// 一条长连接可以发很多请求，只限握手等于没限。
	if h.opts.Limiter != nil && !h.opts.Limiter.Allow() {
		return errorReply(request.ID, request.Op, access.ErrRateLimited), false
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.opts.OperationTimeout)
	defer cancel()

	data, err := ops.Dispatch(ctx, h.opts.Deps, request.Op, request.Args)
	if err != nil {
		status := statusFor(err)
		h.logError("", status, request.Op, err, time.Now())
		return errorReply(request.ID, request.Op, err), false
	}
	return wsReply{Type: "result", ID: request.ID, OK: true, Operation: request.Op, Data: data}, false
}

// errorReply 构造一个错误回包。
func errorReply(id, op string, err error) wsReply {
	status := statusFor(err)
	return wsReply{Type: "error", ID: id, OK: false, Operation: op, Error: newErrBody(err, status)}
}

// writeWS 发送一条 JSON 消息，并设置写超时避免对端不收导致连接卡死。
func (h *httpHandler) writeWS(conn *wsproto.Conn, reply wsReply) error {
	payload, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteText(payload)
}

// wsWriteTimeout 是单条消息的写超时。
const wsWriteTimeout = 30 * time.Second

// isExpectedWSClose 报告错误是否属于正常的会话结束。
func isExpectedWSClose(err error) bool {
	var closeErr *wsproto.CloseError
	if errors.As(err, &closeErr) {
		return true
	}
	// 读超时意味着连接空闲过久，属于正常回收。
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

// logWS 记录 WebSocket 会话日志；不带任何消息内容。
func (h *httpHandler) logWS(msg string, err error) {
	if h.opts.Logger == nil {
		return
	}
	h.opts.Logger.Debug(msg, "原因", err.Error())
}
