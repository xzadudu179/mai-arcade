package ops

import "github.com/xzadudu179/maimai-arcade/internal/model"

// 操作层具名错误码。
var (
	// ErrUnknownOperation 表示请求了未注册的操作。
	ErrUnknownOperation = &model.Error{
		Kind: model.KindParam, Sentinel: model.CodeUnknownOp,
		Msg:  "未知操作",
		Hint: "读 /v1/ops（HTTP）或 hello 消息（WebSocket）获取可用操作",
	}

	// ErrWriteDisabled 表示写操作未启用。它是本服务最重要的安全默认值。
	ErrWriteDisabled = &model.Error{
		Kind: model.KindParam, Sentinel: model.CodeWriteDisabled,
		Msg:  "写操作未启用",
		Hint: "服务默认只读——它持有读取账号成绩并写入查分器的能力；确需同步时用 --allow-write 显式开启",
	}

	// ErrBadArguments 表示参数缺失或不是合法 JSON。
	ErrBadArguments = &model.Error{
		Kind: model.KindParam, Sentinel: model.CodeBadArguments,
		Msg:  "参数不合法",
		Hint: "参数应为 JSON 对象；字段名见 /v1/ops 的操作说明",
	}
)

func init() {
	for _, sentinel := range []*model.Error{ErrUnknownOperation, ErrWriteDisabled, ErrBadArguments} {
		model.RegisterCode(sentinel)
	}
}
