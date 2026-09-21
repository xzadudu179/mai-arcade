package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/access"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/ops"
)

// HTTP API 路径。操作一律走 POST + 请求体：二维码与凭证放在 query 里会被访问日志记下来。
const (
	pathHealth = "/healthz"
	pathOps    = "/v1/ops"
	pathOp     = "/v1/op/{name}"
	pathWS     = "/v1/ws"
)

// NewHTTPHandler 构造带 HTTP 与 WebSocket 两种接入方式的处理器。
func NewHTTPHandler(opts Options) (http.Handler, error) {
	return newHandler(opts, true)
}

// NewHTTPHandlerWithoutWS 构造只提供 HTTP 的处理器。
//
// 不注册 /v1/ws：把攻击面收窄到最小的部署方式。
func NewHTTPHandlerWithoutWS(opts Options) (http.Handler, error) {
	return newHandler(opts, false)
}

// newHandler 装配路由与通用中间件。
//
// 路由与安全策略集中在这里；具体操作由 ops 层提供，本文件不认识任何机台或查分器概念。
func newHandler(opts Options, enableWS bool) (http.Handler, error) {
	if opts.Auth == nil {
		return nil, errors.New("server: HTTP 适配器需要 access.Authenticator")
	}
	opts.normalize()

	h := &httpHandler{opts: opts}

	mux := http.NewServeMux()
	// 存活探针不鉴权：它不接触任何凭证，也不泄露任何业务信息。
	mux.HandleFunc("GET "+pathHealth, h.health)
	mux.HandleFunc("GET "+pathOps, h.protect(h.describe))
	mux.HandleFunc("POST "+pathOp, h.protect(h.runOperation))
	if enableWS {
		// WebSocket 的握手也是一次 HTTP 请求，因此走同一套鉴权与限频。
		mux.HandleFunc(pathWS, h.protect(h.upgradeWebSocket))
	}

	return h.recoverPanics(mux), nil
}

// httpHandler 持有 HTTP 适配器的依赖。
type httpHandler struct {
	opts Options
}

// health 返回存活状态；不鉴权、不返回版本以外的任何信息。
func (h *httpHandler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": h.opts.Deps.Service.Version(),
	}, h.opts.Logger)
}

// describe 返回操作与错误码的自描述，让客户端不必读源码就能知道能调什么、会收到什么码。
func (h *httpHandler) describe(w http.ResponseWriter, r *http.Request) {
	h.info("请求完成", r, http.StatusOK, "describe", 0, time.Now())
	writeJSON(w, http.StatusOK, response{
		OK:        true,
		Operation: "describe",
		Data: map[string]any{
			"version":      h.opts.Deps.Service.Version(),
			"writeEnabled": h.opts.Deps.AllowWrite,
			"operations":   ops.Describe(),
			"errorCodes":   model.Errors(),
			"endpoints":    []string{pathHealth, pathOps, pathOp, pathWS},
		},
	}, h.opts.Logger)
}

// runOperation 执行一次操作。
func (h *httpHandler) runOperation(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	started := time.Now()

	if !h.opts.Sem.TryAcquire() {
		h.fail(w, r, name, access.ErrBusy, started)
		return
	}
	defer h.opts.Sem.Release()

	body, err := access.ReadLimited(r.Body, h.opts.MaxBody)
	if err != nil {
		h.fail(w, r, name, err, started)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.opts.OperationTimeout)
	defer cancel()

	data, err := ops.Dispatch(ctx, h.opts.Deps, name, body)
	if err != nil {
		h.fail(w, r, name, err, started)
		return
	}

	h.info("操作完成", r, http.StatusOK, name, 0, started)
	writeJSON(w, http.StatusOK, response{OK: true, Operation: name, Data: data}, h.opts.Logger)
}

// fail 把错误映射成状态码并写出统一响应。
func (h *httpHandler) fail(w http.ResponseWriter, r *http.Request, name string, err error, started time.Time) {
	status := statusFor(err)

	// 限频时补上 Retry-After，让客户端知道等多久而不是盲目重试。
	if errors.Is(err, access.ErrRateLimited) || modelIsCode(err, model.CodeRateLimited) {
		if retry := h.opts.Limiter.RetryAfter(); retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		}
	}

	h.logError(clientIP(r), status, name, err, started)
	writeJSON(w, status, response{OK: false, Operation: name, Error: newErrBody(err, status)}, h.opts.Logger)
}

// protect 给路由套上鉴权与限频。
//
// 顺序是先鉴权再限频：反过来会让未通过鉴权的流量消耗令牌桶，把合法调用方挡在门外。
func (h *httpHandler) protect(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		subprotocol, err := h.opts.Auth.VerifyRequest(r)
		if err != nil {
			h.fail(w, r, "", err, started)
			return
		}
		if subprotocol != "" {
			// WebSocket 握手需要把子协议回显给浏览器，因此往下传一层。
			r = r.WithContext(context.WithValue(r.Context(), subprotocolKey{}, subprotocol))
		}
		if h.opts.Limiter != nil && !h.opts.Limiter.Allow() {
			h.fail(w, r, "", access.ErrRateLimited, started)
			return
		}
		fn(w, r)
	}
}

// recoverPanics 兜住 panic：绝不能把栈信息或内部细节回给调用方。
func (h *httpHandler) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if h.opts.Logger != nil {
					h.opts.Logger.Error("处理请求时 panic", "路径", r.URL.Path, "原因", recovered)
				}
				writeJSON(w, http.StatusInternalServerError, response{
					OK: false, Error: newErrBody(model.ErrBusiness, http.StatusInternalServerError),
				}, h.opts.Logger)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// info 记录一条不含凭证的请求日志。
//
// 只打操作名、状态、耗时与客户端地址：请求体里含二维码与凭证，一律不进日志。
func (h *httpHandler) info(msg string, r *http.Request, status int, op string, bytes int, started time.Time) {
	if h.opts.Logger == nil {
		return
	}
	h.opts.Logger.Info(msg,
		"op", op,
		"状态码", status,
		"耗时", time.Since(started).Round(time.Millisecond).String())
}

// logError 记录失败请求。
//
// 4xx 是调用方的问题，用 Warn；5xx 需要人工介入，用 Error。
// 客户端地址可能为空（WebSocket 路径上没有原始请求），此时省略该字段。
func (h *httpHandler) logError(client string, status int, op string, err error, started time.Time) {
	if h.opts.Logger == nil {
		return
	}
	level := h.opts.Logger.Warn
	if status >= http.StatusInternalServerError {
		level = h.opts.Logger.Error
	}

	attrs := []any{
		"op", op,
		"状态码", status,
		"耗时", time.Since(started).Round(time.Millisecond).String(),
		"原因", err.Error(),
	}
	if client != "" {
		attrs = append(attrs, "客户端", client)
	}
	level("请求失败", attrs...)
}

// clientIP 取客户端地址，供限频与滥用排查使用。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// modelIsCode 报告错误是否带指定码；用于在不 import protocol 的前提下判断限频。
func modelIsCode(err error, code model.Code) bool {
	got, ok := model.CodeOf(err)
	return ok && got == code
}
