package model

import (
	"errors"
	"sort"
	"sync"
)

// Code 是对外稳定的机器可读错误码。
//
// 它与 Kind 的分工：Kind 只有三档、决定退出码；Code 精确到具体原因，
// 供调用方（CLI、HTTP 客户端、bot）区分处理。码值一旦发布就不应改动。
type Code string

// 协议层错误码。
const (
	CodeSGIDFormat      Code = "sgid_format"
	CodeSGIDExpired     Code = "sgid_expired"
	CodeEmptyResponse   Code = "empty_response"
	CodeDecrypt         Code = "decrypt"
	CodeServerPlaintext Code = "server_plaintext"
	CodeVersionMismatch Code = "version_mismatch"
	CodeAlreadyLoggedIn Code = "already_logged_in"
	CodeTimeout         Code = "timeout"
	CodeAimeUnavailable Code = "aime_unavailable"
	CodeAimeQRRejected  Code = "aime_qr_rejected"
	CodeAimeBadSig      Code = "aime_bad_signature"

	CodeVersionUnsupported Code = "version_unsupported"
	CodeConfigBad          Code = "config_invalid"
)

// 查分器侧错误码。
const (
	CodeSiteCredential Code = "site_credential_invalid"
	CodeSiteStatus     Code = "site_unexpected_status"
	CodeSiteNoSongs    Code = "site_chart_index_unavailable"
	CodeSiteUnmappable Code = "site_scores_unmappable"
	CodeSiteRecordsBad Code = "site_records_unparsable"
	CodeSiteUploadFail Code = "site_upload_failed"
	CodeUnknownSite    Code = "unknown_site"
)

// 通用错误码。
const (
	CodeUsage    Code = "usage"
	CodeNetwork  Code = "network"
	CodeBusiness Code = "business"
	CodeParam    Code = "param"
	CodeInternal Code = "internal"
)

// 接入层错误码：HTTP / WebSocket 适配器自有。
const (
	CodeUnauthorized  Code = "unauthorized"
	CodeRateLimited   Code = "rate_limited"
	CodeTooLarge      Code = "payload_too_large"
	CodeWriteDisabled Code = "write_disabled"
	CodeUnknownOp     Code = "unknown_operation"
	CodeBadArguments  Code = "bad_arguments"
	CodeBusy          Code = "busy"
)

// DeclaredCodes 列出本包声明的全部错误码常量。
//
// 登记是各包在 init 里各自完成的（说明文字因此不必写两遍），代价是目录的完整性
// 取决于哪些包被链接进来。这个函数把「应该有哪些码」显式列出来，
// 好让链接齐全的二进制（CLI 与服务）能用一条测试断言目录没有漏项。
func DeclaredCodes() []Code {
	return []Code{
		// 协议层
		CodeSGIDFormat, CodeSGIDExpired, CodeEmptyResponse, CodeDecrypt, CodeServerPlaintext,
		CodeVersionMismatch, CodeAlreadyLoggedIn, CodeTimeout, CodeAimeUnavailable,
		CodeAimeQRRejected, CodeAimeBadSig, CodeVersionUnsupported, CodeConfigBad,
		// 查分器侧
		CodeSiteCredential, CodeSiteStatus, CodeSiteNoSongs, CodeSiteUnmappable,
		CodeSiteRecordsBad, CodeSiteUploadFail, CodeUnknownSite,
		// 通用
		CodeUsage, CodeNetwork, CodeBusiness, CodeParam, CodeInternal,
		// 接入层
		CodeUnauthorized, CodeRateLimited, CodeTooLarge, CodeWriteDisabled,
		CodeUnknownOp, CodeBadArguments, CodeBusy,
	}
}

// CodeInfo 是一个错误码的对外说明。
type CodeInfo struct {
	Code    Code   `json:"code"`
	Kind    Kind   `json:"kind"`
	Meaning string `json:"meaning"`
	Hint    string `json:"hint,omitempty"`
}

var (
	codesMu sync.RWMutex
	codes   = make(map[Code]CodeInfo)
)

func init() {
	// 通用码由本包直接登记：它们不属于任何单一协议层。
	for _, info := range []CodeInfo{
		{Code: CodeUsage, Kind: KindParam, Meaning: "参数用法错误", Hint: "检查命令行参数或请求体字段"},
		{Code: CodeParam, Kind: KindParam, Meaning: "参数错误", Hint: "检查输入取值"},
		{Code: CodeNetwork, Kind: KindNetwork, Meaning: "网络错误", Hint: "检查网络与代理出口"},
		{Code: CodeBusiness, Kind: KindBusiness, Meaning: "业务失败", Hint: "按错误信息处理"},
		{Code: CodeInternal, Kind: KindBusiness, Meaning: "内部错误", Hint: "保留日志排查"},
	} {
		registerCode(info)
	}
}

// RegisterCode 用哨兵自身登记对外说明，使错误码表自动保持完整。
//
// 说明直接取自哨兵的 Msg / Hint，因此不需要在两处维护同一句话。
// 同一码重复登记会 panic：那属于复制粘贴出的编码错误，应当在启动时就暴露。
func RegisterCode(e *Error) {
	if e == nil || e.Sentinel == "" {
		// 分类哨兵（ErrBusiness / ErrNetwork / ErrParam）没有独立码，不参与登记。
		return
	}
	registerCode(CodeInfo{
		Code:    e.Sentinel,
		Kind:    e.Kind,
		Meaning: e.Msg,
		Hint:    e.Hint,
	})
}

// RegisterCodeInfo 直接登记一条错误码说明，用于没有哨兵的一次性错误码。
func RegisterCodeInfo(info CodeInfo) { registerCode(info) }

func registerCode(info CodeInfo) {
	if info.Code == "" {
		panic("model: 登记错误码时 Code 不能为空")
	}
	codesMu.Lock()
	defer codesMu.Unlock()
	if existing, ok := codes[info.Code]; ok {
		panic("model: 错误码重复登记 " + string(info.Code) +
			"（已有含义 " + existing.Meaning + "，新含义 " + info.Meaning + "）")
	}
	codes[info.Code] = info
}

// Errors 返回全部已登记的错误码，按码值排序以保证输出稳定。
func Errors() []CodeInfo {
	codesMu.RLock()
	out := make([]CodeInfo, 0, len(codes))
	for _, info := range codes {
		out = append(out, info)
	}
	codesMu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// LookupCode 按码取出对外说明。
func LookupCode(c Code) (CodeInfo, bool) {
	codesMu.RLock()
	defer codesMu.RUnlock()
	info, ok := codes[c]
	return info, ok
}

// CodeOf 取出错误链上最外层的对外错误码。
//
// 分类哨兵（只有 Kind 没有码）返回 false，调用方应改用 Kind 兜底。
func CodeOf(err error) (Code, bool) {
	var e *Error
	if !errors.As(err, &e) || e.Sentinel == "" {
		return "", false
	}
	return e.Sentinel, true
}
