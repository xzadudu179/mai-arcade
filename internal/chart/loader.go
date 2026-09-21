package chart

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// DefaultBaseURL 是水鱼查分器的 API 根地址。
const DefaultBaseURL = "https://www.diving-fish.com/api/maimaidxprober"

// musicDataPath 是获取全曲库的端点，无需任何鉴权。
const musicDataPath = "/music_data"

// LoaderOptions 是构造 Loader 的全部依赖。
type LoaderOptions struct {
	// HTTP 是传输客户端，必填。
	HTTP *transport.Client

	// BaseURL 是查分器 API 根地址，为空取 DefaultBaseURL。
	BaseURL string

	// Cache 是本地曲目缓存，nil 表示每次都回源。
	Cache *Cache

	// Logger 用于记录拉取结果，nil 表示不记录。
	Logger *slog.Logger
}

// Loader 从查分器拉取曲目数据并构造索引。
type Loader struct {
	hc      *transport.Client
	baseURL string
	cache   *Cache
	logger  *slog.Logger
}

// NewLoader 构造 Loader。
func NewLoader(opts LoaderOptions) (*Loader, error) {
	if opts.HTTP == nil {
		return nil, fmt.Errorf("chart.Loader 需要 transport.Client")
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return &Loader{hc: opts.HTTP, baseURL: base, cache: opts.Cache, logger: opts.Logger}, nil
}

// Load 取全曲库并构造索引：优先用本地缓存，未命中或过期才回源。
func (l *Loader) Load(ctx context.Context) (*Index, error) {
	body, err := l.fetch(ctx)
	if err != nil {
		return nil, err
	}

	songs, err := SongsFromMusicData(body)
	if err != nil {
		return nil, err
	}
	index := NewIndex(songs)
	if l.logger != nil {
		l.logger.Debug("曲目索引就绪", "曲目数", index.Len())
	}
	return index, nil
}

// fetch 返回曲目数据的原始 JSON：先看缓存，未命中再回源并写入缓存。
//
// 缓存读失败一律当作未命中；回源成功后写缓存失败也只记日志——
// 缓存是加速手段，任何情况下都不该让主流程失败。
func (l *Loader) fetch(ctx context.Context) ([]byte, error) {
	if l.cache.enabled() {
		if body, ok := l.cache.Load(); ok {
			return body, nil
		}
	}

	resp, err := l.hc.Get(ctx, l.baseURL+musicDataPath, nil)
	if err != nil {
		return nil, fmt.Errorf("拉取曲目数据: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("拉取曲目数据失败: HTTP %d", resp.StatusCode)
	}

	l.cache.Store(resp.Body)
	return resp.Body, nil
}
