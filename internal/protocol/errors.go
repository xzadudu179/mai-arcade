package protocol

import (
	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// 错误分类与错误类型复用 model 里的实现：sync 与 service 也要给错误贴分类，
// 而 sync 不允许依赖 protocol，因此这份共享词汇下沉到叶子包。此处以别名暴露，
// 调用方仍可只写 protocol.ErrNetwork 而不必关心它定义在哪。
type (
	// Kind 是错误分类，数字即退出码。
	Kind = model.Kind

	// Error 是带分类、机器可读码与补救提示的错误。
	Error = model.Error
)

// 错误分类取值。
const (
	KindBusiness = model.KindBusiness
	KindNetwork  = model.KindNetwork
	KindParam    = model.KindParam
)

// 分类哨兵。
var (
	ErrBusiness = model.ErrBusiness
	ErrNetwork  = model.ErrNetwork
	ErrParam    = model.ErrParam
)

// 协议层具名哨兵。
var (
	ErrSGIDFormat = &Error{
		Kind: KindParam, Sentinel: model.CodeSGIDFormat,
		Msg:  "二维码格式非法",
		Hint: "确认从公众号复制的完整内容：84 字符、以 SGWCMAID 开头、后 64 位为十六进制",
	}
	ErrSGIDExpired = &Error{
		Kind: KindParam, Sentinel: model.CodeSGIDExpired,
		Msg:  "二维码已过期",
		Hint: "二维码有效期约 10 分钟，请重新从公众号获取",
	}
	ErrEmptyResponse = &Error{
		Kind: KindNetwork, Sentinel: model.CodeEmptyResponse,
		Msg: "标题服务器返回 0 字节响应（TLS 正常）",
		Hint: "两种成因：协议版本已不被接受，或出口 IP 被阻断。用 probe 逐个尝试版本即可区分；" +
			"换 IP / 重启光猫 / 等 48–72 小时只对后者有效",
	}
	ErrDecrypt = &Error{
		Kind: KindParam, Sentinel: model.CodeDecrypt,
		Msg:  "响应解密失败",
		Hint: "协议参数版本不匹配，换 --version 重试（如 --version 1.55）",
	}
	ErrServerPlaintext = &Error{
		Kind: KindParam, Sentinel: model.CodeServerPlaintext,
		Msg: "服务端返回明文响应（不是加密体），通常是请求字段非法触发了服务端异常",
		Hint: "看错误里的 HTTP 状态码与响应片段：500 + Tomcat 页说明请求已到达业务逻辑但字段不被接受，" +
			"优先检查必填字段（clientId / placeId / token / dateTime 及其单位），而不是换协议版本",
	}
	ErrVersionMismatch = &Error{
		Kind: KindParam, Sentinel: model.CodeVersionMismatch,
		Msg:  "响应解密成功但不是合法 JSON",
		Hint: "协议参数版本不匹配，换 --version 重试（如 --version 1.55）",
	}
	ErrAlreadyLoggedIn = &Error{
		Kind: KindBusiness, Sentinel: model.CodeAlreadyLoggedIn,
		Msg:  "账号已在登录状态",
		Hint: "该账号的服务器会话未释放，等会话超时（约 15 分钟）后重试；不要反复重试以免加重封禁",
	}
	ErrTimeout = &Error{
		Kind: KindNetwork, Sentinel: model.CodeTimeout,
		Msg:  "请求超时",
		Hint: "检查网络与代理；机台接口对出口 IP 敏感，代理出口同样可能被拦",
	}
	ErrAimeUnavailable = &Error{
		Kind: KindNetwork, Sentinel: model.CodeAimeUnavailable,
		Msg:  "AimeDB 无有效响应",
		Hint: "确认能直连 http://ai.sys-allnet.cn；中间网络设备改写响应会导致解析失败",
	}
	ErrAimeQRRejected = &Error{
		Kind: KindParam, Sentinel: model.CodeAimeQRRejected,
		Msg:  "AimeDB 拒绝该二维码（已过期或不存在）",
		Hint: "重新从公众号获取二维码；二维码有效期约 10 分钟",
	}
	ErrUnsupportedVersion = &Error{
		Kind: KindParam, Sentinel: model.CodeVersionUnsupported,
		Msg:  "不支持的协议版本",
		Hint: "换一个 --version 重试；可用版本见 Mai-Encoding 对照表",
	}
	ErrBadConfig = &Error{
		Kind: KindParam, Sentinel: model.CodeConfigBad,
		Msg:  "配置不合法",
		Hint: "检查构造函数参数（代理地址、超时、协议参数表）",
	}
	ErrAimeBadSignature = &Error{
		Kind: KindParam, Sentinel: model.CodeAimeBadSig,
		Msg:  "AimeDB 拒绝请求签名",
		Hint: "检查 chipID 与本机时区：时间戳按本地时区生成，时区不对会导致签名不匹配",
	}
)

// wrap 给哨兵补上原始码与原因，返回新的错误值（不改动哨兵本身）。
func wrap(sentinel *Error, code string, err error) *Error {
	return &Error{
		Kind:     sentinel.Kind,
		Sentinel: sentinel.Sentinel,
		Code:     code,
		Msg:      sentinel.Msg,
		Hint:     sentinel.Hint,
		Err:      err,
	}
}

// businessErr 构造一个带服务端码的业务错误。
func businessErr(msg, code, hint string) *Error {
	return &Error{Kind: KindBusiness, Msg: msg, Code: code, Hint: hint}
}

// IsKind 报告 err 是否属于指定分类。
func IsKind(err error, k Kind) bool { return model.IsKind(err, k) }

// sentinels 是协议层全部具名错误码；集中登记一次，保证错误码表不会漏项。
var sentinels = []*Error{
	ErrSGIDFormat,
	ErrSGIDExpired,
	ErrEmptyResponse,
	ErrDecrypt,
	ErrServerPlaintext,
	ErrVersionMismatch,
	ErrAlreadyLoggedIn,
	ErrTimeout,
	ErrAimeUnavailable,
	ErrAimeQRRejected,
	ErrAimeBadSignature,
	ErrUnsupportedVersion,
	ErrBadConfig,
}

func init() {
	for _, sentinel := range sentinels {
		model.RegisterCode(sentinel)
	}
}
