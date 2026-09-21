// Package server 是接入层：把与接入方式无关的操作层接到具体的传输上。
//
// 目前提供 HTTP 与 WebSocket 两种适配器，它们共用同一份鉴权、限频、限额与错误映射
// （见 access 与 ops）。新增一种接入方式 = 新增一个 <name>.go，操作定义与安全策略都不用动。
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/access"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/ops"
)

// Options 是两种适配器共用的配置。
type Options struct {
	// Deps 是操作层依赖。
	Deps ops.Deps

	// Auth 是鉴权器，必填。
	Auth *access.Authenticator

	// Limiter 是全局限频器，nil 表示不限频。
	Limiter *access.RateLimiter

	// Sem 是并发上限，nil 表示不限制。
	Sem access.Semaphore

	// MaxBody 是请求体上限，为零取 access.DefaultMaxBody。
	MaxBody int64

	// OperationTimeout 是单次操作的服务端超时上限。
	//
	// 它必须大于编排层允许的最长会话，否则会在机台会话收尾（登出）之前就把请求掐掉。
	OperationTimeout time.Duration

	// WSIdleTimeout 是 WebSocket 连接的空闲上限，超时即回收，避免连接越积越多。
	WSIdleTimeout time.Duration

	// Logger 是日志出口。
	Logger *slog.Logger
}

// normalize 填入缺省值。
func (o *Options) normalize() {
	if o.MaxBody <= 0 {
		o.MaxBody = access.DefaultMaxBody
	}
	if o.OperationTimeout <= 0 {
		o.OperationTimeout = 15 * time.Minute
	}
	if o.WSIdleTimeout <= 0 {
		o.WSIdleTimeout = 5 * time.Minute
	}
}

// response 是所有接入方式共用的成功响应信封。
type response struct {
	OK        bool     `json:"ok"`
	Operation string   `json:"operation"`
	Data      any      `json:"data,omitempty"`
	Error     *errBody `json:"error,omitempty"`
}

// errBody 是统一的错误表示。
//
// 字段分工与 CLI 一致：kind 决定大类，code 精确到原因（客户端应优先按 code 分支），
// detail 是上游返回的原始码。
type errBody struct {
	Code    string `json:"code,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`

	// Status 同时出现在响应体与 HTTP 状态行里，方便日志与客户端一致地判断。
	Status int `json:"status"`
}

// newErrBody 把错误展开成响应体。
func newErrBody(err error, status int) *errBody {
	body := &errBody{Kind: "unclassified", Message: err.Error(), Status: status}

	if code, ok := model.CodeOf(err); ok {
		body.Code = string(code)
	}
	var me *model.Error
	if errors.As(err, &me) {
		body.Kind = me.Kind.String()
		body.Detail = me.Code
		body.Hint = me.Hint
	}
	return body
}

// writeJSON 写出 JSON 响应；编码失败时退化为纯文本，避免静默返回空体。
func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 本服务只应被自己的客户端访问，禁止中间层缓存带凭证的响应。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil && logger != nil {
		logger.Error("写出响应失败", "原因", err.Error())
	}
}

// codeStatus 给出错误码到 HTTP 状态码的完整映射。
//
// 表驱动而非按 Kind 兜底：这样每个码的状态都是被显式决定过的，
// 新增码时 TestEveryRegisteredCodeHasStatus 会强制你补上这一行。
var codeStatus = map[model.Code]int{
	// 参数类：调用方改一下就能成功
	model.CodeUsage:              http.StatusBadRequest,
	model.CodeParam:              http.StatusBadRequest,
	model.CodeBadArguments:       http.StatusBadRequest,
	model.CodeSGIDFormat:         http.StatusBadRequest,
	model.CodeSiteCredential:     http.StatusBadRequest,
	model.CodeUnknownSite:        http.StatusBadRequest,
	model.CodeAimeQRRejected:     http.StatusBadRequest,
	model.CodeAimeBadSig:         http.StatusBadRequest,
	model.CodeVersionUnsupported: http.StatusBadRequest,

	// 状态类：请求本身没错，但当前状态不允许
	model.CodeAlreadyLoggedIn: http.StatusConflict,
	model.CodeBusiness:        http.StatusConflict,

	// 凭证与权限
	model.CodeUnauthorized:  http.StatusUnauthorized,
	model.CodeWriteDisabled: http.StatusForbidden,

	// 资源与限流
	model.CodeUnknownOp:   http.StatusNotFound,
	model.CodeSGIDExpired: http.StatusGone,
	model.CodeTooLarge:    http.StatusRequestEntityTooLarge,
	model.CodeRateLimited: http.StatusTooManyRequests,
	model.CodeBusy:        http.StatusServiceUnavailable,

	// 上游与网络：本服务无错，问题在链路另一端
	model.CodeNetwork:         http.StatusBadGateway,
	model.CodeEmptyResponse:   http.StatusBadGateway,
	model.CodeTimeout:         http.StatusGatewayTimeout,
	model.CodeAimeUnavailable: http.StatusBadGateway,
	model.CodeSiteStatus:      http.StatusBadGateway,
	model.CodeSiteNoSongs:     http.StatusBadGateway,
	model.CodeSiteUnmappable:  http.StatusBadGateway,
	model.CodeSiteRecordsBad:  http.StatusBadGateway,
	model.CodeSiteUploadFail:  http.StatusBadGateway,
	// 解密失败与「能解密但不是 JSON」都指向协议参数版本不对，对客户端而言是上游不可用。
	model.CodeDecrypt:         http.StatusBadGateway,
	model.CodeVersionMismatch: http.StatusBadGateway,

	// 本服务自身配置有误
	model.CodeConfigBad: http.StatusInternalServerError,
	model.CodeInternal:  http.StatusInternalServerError,
	// 上游回了明文错误页：本服务发出的请求字段不被接受，属自身实现/配置问题。
	model.CodeServerPlaintext: http.StatusInternalServerError,
}

// statusFor 把错误映射成 HTTP 状态码。
func statusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if code, ok := model.CodeOf(err); ok {
		if status, ok := codeStatus[code]; ok {
			return status
		}
	}
	// 没有具名码时按分类兜底，保证不会出现「500 但其实是我传错了参数」。
	var me *model.Error
	if errors.As(err, &me) {
		switch me.Kind {
		case model.KindParam:
			return http.StatusBadRequest
		case model.KindNetwork:
			return http.StatusBadGateway
		case model.KindBusiness:
			return http.StatusConflict
		}
	}
	return http.StatusInternalServerError
}
