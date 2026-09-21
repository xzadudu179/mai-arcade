// Package ops 定义与接入方式无关的操作层：服务能做什么，以及每个操作怎么被调用。
//
// 它不引入任何传输概念（没有 HTTP、没有 WebSocket、没有帧），请求参数是 JSON 字节，
// 返回值是任意结构。接入方式的差异全部留在 internal/server：
// 新增一种接入方式只需新增一个适配器，操作定义与安全策略都不用改。
package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// Deps 是操作层运行所需的依赖。
type Deps struct {
	// Service 是编排层门面，操作只通过它与机台和查分器交互。
	Service *service.Service

	// AllowWrite 决定是否放行会改动外部数据的操作。
	//
	// 默认关闭：本服务持有「读账号成绩并写入查分器」的能力，暴露到网络上时
	// 只读应当是不用想就能选的安全默认值，写操作必须由运维显式打开。
	AllowWrite bool

	// Logger 是脱敏后的日志出口。
	Logger *slog.Logger
}

// RunFunc 执行一次操作。args 是调用方给的 JSON 参数（可为空）。
type RunFunc func(ctx context.Context, deps Deps, args json.RawMessage) (any, error)

// Operation 是一个操作的完整定义。
type Operation struct {
	// Name 是操作名，也是接入层路由与消息里的标识。
	Name string

	// Summary 是给人类看的用途说明，用于自描述端点。
	Summary string

	// Mutating 标记会改动外部数据的操作；为真时要求 Deps.AllowWrite。
	Mutating bool

	// RequiresCredential 标记是否必须传入第三方凭证（如查分器 Import-Token）。
	RequiresCredential bool

	// Run 是操作实现。
	Run RunFunc
}

// registry 是操作注册表：新增操作 = 新增文件 + 注册一行。
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Operation)
)

// Register 注册一个操作；重名会 panic，属于编码错误。
func Register(op Operation) {
	if op.Name == "" || op.Run == nil {
		panic("ops: Register 需要非空的 Name 与 Run")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[op.Name]; exists {
		panic("ops: 重复注册操作 " + op.Name)
	}
	registry[op.Name] = op
}

// Lookup 按名取出操作。
func Lookup(name string) (Operation, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	op, ok := registry[name]
	return op, ok
}

// All 返回全部操作，按名排序以保证输出稳定。
func All() []Operation {
	registryMu.RLock()
	out := make([]Operation, 0, len(registry))
	for _, op := range registry {
		out = append(out, op)
	}
	registryMu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Descriptor 是操作的自描述信息，供 /v1/ops 与 WebSocket 的 hello 消息使用。
type Descriptor struct {
	Name               string `json:"name"`
	Summary            string `json:"summary"`
	Mutating           bool   `json:"mutating"`
	RequiresCredential bool   `json:"requiresCredential"`
}

// Describe 返回全部操作的自描述信息。
func Describe() []Descriptor {
	all := All()
	out := make([]Descriptor, 0, len(all))
	for _, op := range all {
		out = append(out, Descriptor{
			Name:               op.Name,
			Summary:            op.Summary,
			Mutating:           op.Mutating,
			RequiresCredential: op.RequiresCredential,
		})
	}
	return out
}

// Dispatch 执行一次操作：校验存在性、写保护与参数，再调用实现。
//
// 所有接入方式都经由这里，因此安全策略与错误分类只有一份实现。
func Dispatch(ctx context.Context, deps Deps, name string, args json.RawMessage) (any, error) {
	op, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w：%q，可用操作 %v", ErrUnknownOperation, name, Names())
	}
	if op.Mutating && !deps.AllowWrite {
		return nil, fmt.Errorf("%w：%s", ErrWriteDisabled, op.Name)
	}
	if len(args) > 0 && !json.Valid(args) {
		return nil, fmt.Errorf("%w: 参数不是合法 JSON", ErrBadArguments)
	}
	return op.Run(ctx, deps, args)
}

// Names 返回全部操作名，顺序稳定。
func Names() []string {
	all := All()
	out := make([]string, 0, len(all))
	for _, op := range all {
		out = append(out, op.Name)
	}
	return out
}

// decodeArgs 解析操作参数；空参数按零值处理。
func decodeArgs(args json.RawMessage, into any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("%w: %v", ErrBadArguments, err)
	}
	return nil
}

// sgidArgs 是只含二维码的操作参数。
type sgidArgs struct {
	// SGID 是玩家二维码原文。它不落盘、不进日志。
	SGID string `json:"sgid"`
}

// requireSGID 取出二维码参数并完成格式校验，返回可直接交给 protocol 的值。
//
// 校验放在这里而不是各操作里：二维码是账号凭证，格式检查必须无一例外地发生。
func requireSGID(args json.RawMessage) (protocol.SGID, error) {
	var parsed sgidArgs
	if err := decodeArgs(args, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.SGID) == "" {
		return "", fmt.Errorf("%w: 缺少 sgid 参数", ErrBadArguments)
	}
	return protocol.NewSGID(parsed.SGID)
}
