package model

import (
	"errors"
	"fmt"
)

// Kind 是错误分类，直接决定 CLI 退出码：业务 1 / 网络或阻断 2 / 参数 3。
//
// 它放在本包，是因为 protocol、sync、service 都需要给错误贴分类，
// 而 sync 不允许依赖 protocol；把这份共享词汇放在最底层的叶子里，
// 依赖方向依然是单向的，且各层不必互相 import。
type Kind uint8

// 错误分类取值，数字即退出码。
const (
	KindBusiness Kind = 1
	KindNetwork  Kind = 2
	KindParam    Kind = 3
)

// ExitCode 返回该分类对应的进程退出码。
func (k Kind) ExitCode() int { return int(k) }

// String 返回分类名，供日志与结构化输出使用。
func (k Kind) String() string {
	switch k {
	case KindBusiness:
		return "business"
	case KindNetwork:
		return "network"
	case KindParam:
		return "param"
	default:
		return "unclassified"
	}
}

// MarshalJSON 输出分类名而不是数字，让错误码表与结构化输出对人类可读。
func (k Kind) MarshalJSON() ([]byte, error) {
	return []byte(`"` + k.String() + `"`), nil
}

// Error 是带分类、机器可读码与「下一步怎么办」提示的错误。
//
// 用户可见的错误必须说清怎么补救，而不是只抛一个 code。
type Error struct {
	Kind Kind

	// Sentinel 是错误码：非空时该错误是一个具名哨兵，errors.Is 按它匹配，
	// 同时它也是对外暴露的稳定码（见 Codes）。为空时 errors.Is 退化为按 Kind 匹配。
	Sentinel Code

	// Code 是协议或服务端返回的原始码，如 "errorID=1"、"returnCode=100"。
	Code string

	// Msg 是面向用户的一句话描述。
	Msg string

	// Hint 是补救建议。
	Hint string

	// Err 是被包装的下层错误。
	Err error
}

// Error 实现 error，输出「描述 + 码 + 建议 + 原因链」。
func (e *Error) Error() string {
	s := e.Msg
	if s == "" {
		s = string(e.Sentinel)
	}
	if e.Code != "" {
		s = fmt.Sprintf("%s [%s]", s, e.Code)
	}
	if e.Hint != "" {
		s += "；建议：" + e.Hint
	}
	if e.Err != nil {
		s += "；原因：" + e.Err.Error()
	}
	return s
}

// Unwrap 暴露下层错误，保留链路。
func (e *Error) Unwrap() error { return e.Err }

// Is 让 errors.Is 既能按具名哨兵匹配，也能按分类匹配。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if t.Sentinel != "" {
		return e.Sentinel == t.Sentinel
	}
	return e.Kind == t.Kind
}

// 分类哨兵：任何属于该分类的错误都能被 errors.Is 命中，CLI 据此定退出码。
var (
	ErrBusiness = &Error{Kind: KindBusiness, Msg: "业务失败"}
	ErrNetwork  = &Error{Kind: KindNetwork, Msg: "网络错误"}
	ErrParam    = &Error{Kind: KindParam, Msg: "参数错误"}
)

// Errorf 构造一个带分类的自定义错误。
func Errorf(kind Kind, sentinel Code, code, msg, hint string, err error) *Error {
	return &Error{Kind: kind, Sentinel: sentinel, Code: code, Msg: msg, Hint: hint, Err: err}
}

// IsKind 报告 err 是否属于指定分类。
func IsKind(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}
