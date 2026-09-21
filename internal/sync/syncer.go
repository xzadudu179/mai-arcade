// Package sync 把机台成绩推送到查分器。
//
// 它只懂查分器，不懂机台协议：输入是归一化的 model.Score，输出是各站自己的上传请求。
package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// Syncer 是一个查分器的上传能力。
//
// 接口由调用方定义：合并策略属于各实现自己的事（水鱼必须合并，否则会清空已有 FC/FS），
// 调用方只管把成绩和凭证交给它。
type Syncer interface {
	// Name 返回注册名，也是 CLI --site 的取值。
	Name() string

	// Upload 把成绩上传到该查分器；credential 是该站的凭证（水鱼为 Import-Token）。
	Upload(ctx context.Context, scores []model.Score, credential string) error
}

// Deps 是构造查分器实现所需的共享依赖。
//
// 水鱼需要按歌名定位曲目，因此必须能查到曲目元数据；不需要曲目的站点可以忽略 Songs。
type Deps struct {
	// HTTP 是传输客户端。
	HTTP *transport.Client

	// Songs 提供 musicId 到曲目元数据的索引。
	Songs chart.Source

	// BaseURL 是 API 根地址，为空时各实现使用自己的默认值。
	BaseURL string

	// Logger 用于记录脱敏后的步骤日志，nil 表示不记录。
	Logger *slog.Logger
}

// Factory 按依赖构造一个查分器实现。
type Factory func(Deps) Syncer

// registry 是查分器注册表：新增站点只需在自己的文件里 init() 注册一行。
var registry = make(map[string]Factory)

// Register 注册一个查分器实现；重名会 panic，属于编码错误而非运行时状况。
func Register(name string, f Factory) {
	if _, exists := registry[name]; exists {
		panic("sync: 重复注册查分器 " + name)
	}
	if name == "" || f == nil {
		panic("sync: Register 需要非空的 name 与 factory")
	}
	registry[name] = f
}

// ErrUnknownSite 是站点名不在注册表里的错误。
var ErrUnknownSite = &model.Error{
	Kind: model.KindParam, Sentinel: model.CodeUnknownSite,
	Msg:  "未知查分器",
	Hint: "用 --help 查看可用取值，或读 /v1/ops 的自描述",
}

func init() { model.RegisterCode(ErrUnknownSite) }

// unknownSiteError 在哨兵上补出具体站点名与可用取值。
func unknownSiteError(name string) *model.Error {
	e := *ErrUnknownSite
	e.Msg = "未知查分器 " + name
	e.Hint = "可用取值：" + fmt.Sprint(Names())
	return &e
}

// Registered 报告某个查分器是否已注册。
//
// 调用方可以据此在做起网络请求、占用机台会话之前，先把写错的站点名拦下来。
func Registered(name string) bool {
	_, ok := registry[name]
	return ok
}

// New 按注册名构造查分器实现。
func New(name string, deps Deps) (Syncer, error) {
	f, ok := registry[name]
	if !ok {
		return nil, unknownSiteError(name)
	}
	return f(deps), nil
}

// Names 返回已注册的查分器名，顺序稳定以便提示与测试。
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
