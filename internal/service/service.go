// Package service 编排「扫码 → 登录 → 取分 → 登出 → 同步」，是调用方唯一需要接触的层。
//
// 它同时依赖 protocol 与 sync：前者给机台成绩，后者把成绩送去查分器，编排职责只落在这里。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	sitesync "github.com/xzadudu179/maimai-arcade/internal/sync"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// 会话相关的时间常量。
const (
	// DefaultSessionTimeout 是单次会话的总时长上限。
	//
	// 机台服务器会话硬超时是 15 分钟，超时未登出就进「小黑屋」；这里留出余量，
	// 保证整个「登录→取分→登出」序列一定能跑完并发出登出请求。
	DefaultSessionTimeout = 10 * time.Minute

	// logoutTimeout 是登出单独的超时。它不继承会话上下文，避免会话超时后连登出都发不出去。
	logoutTimeout = 20 * time.Second

	// DefaultTimeout 是单次 HTTP 请求超时。
	DefaultTimeout = 30 * time.Second
)

// Options 是构造 Service 的全部依赖，一律显式传入。
type Options struct {
	// Version 是协议版本号，为空取 protocol.DefaultVersion。
	Version string

	// ProxyURL 是 HTTP 代理地址，为空表示直连。
	ProxyURL string

	// Timeout 是单次请求超时，为零取 DefaultTimeout。
	Timeout time.Duration

	// SessionTimeout 是单次会话总时长上限，为零取 DefaultSessionTimeout。
	SessionTimeout time.Duration

	// TitleBaseURL 是标题服务器根地址，为空取 protocol.DefaultTitleBaseURL。
	TitleBaseURL string

	// AimeURL 是 AimeDB 接口地址，为空取 protocol.AimeDBURL。
	AimeURL string

	// ChartBaseURL 是查分器 API 根地址，为空取 chart.DefaultBaseURL。
	ChartBaseURL string

	// ChartCacheDir 是曲目数据的本地缓存目录，为空表示不缓存。
	//
	// 曲目数据约 1 MB，同步一次就要拉一遍；定数与新旧曲标记只在版本更新时变化，
	// 因此缓存收益明显。缓存失败不影响主流程。
	ChartCacheDir string

	// ChartCacheTTL 是曲目缓存有效期，为零取 chart.DefaultCacheTTL。
	ChartCacheTTL time.Duration

	// AutoVersion 为真时在首次使用前自动探测可用的协议版本。
	AutoVersion bool

	// DisableReuse 关闭同一二维码的结果复用（见 reuse.go）；默认开启复用。
	DisableReuse bool

	// ReuseTTL 是结果复用的时间上限，为零取 defaultReuseTTL。
	ReuseTTL time.Duration

	// Logger 是结构化日志出口，nil 表示不记录。
	Logger *slog.Logger
}

// titleSession 是 service 需要机台客户端提供的能力。
//
// 抽成接口是为了能在测试里替换掉真实机台——「取分失败也必须登出」这条验收要求
// 只有能观察到登出调用才验证得了。接口定义在调用方，符合 §5.3 的约定。
type titleSession interface {
	Login(ctx context.Context, cred protocol.Credential, regionID int) error
	Logout(ctx context.Context) error
	Music(ctx context.Context) ([]model.Score, error)
	Profile(ctx context.Context) (protocol.Profile, error)
}

// titleFactory 按参数构造一次会话的客户端。
type titleFactory func(protocol.TitleOptions) (titleSession, error)

// Service 是门面对象。它持有跨账号的串行锁与共享的 HTTP 客户端。
type Service struct {
	opts   Options
	hc     *transport.Client
	logger *slog.Logger

	// version 会被 DetectVersion 改写，读写都要持锁。
	versionMu sync.RWMutex
	version   protocol.Version
	detected  bool

	// versionProbeDisabled 让测试关掉探测，避免引入网络依赖。
	versionProbeDisabled bool

	// cache 复用同一二维码的结果；opts.DisableReuse 为真时留空。
	cache *resultCache

	// newTitle 构造机台客户端，测试里可替换。
	newTitle titleFactory

	// mu 串行化账号操作。锁的粒度是全局而非按 userId：
	// 换账号成功前拿不到 userId，而 AimeDB 换账号本身也不能与同一账号的会话并发。
	// 本项目是单实例、低频调用，全局串行是最省心的正确做法。
	mu sync.Mutex
}

// New 解析版本参数并构造 Service。
func New(opts Options) (*Service, error) {
	version, err := protocol.LookupVersion(opts.Version)
	if err != nil {
		return nil, err
	}
	if opts.SessionTimeout <= 0 {
		opts.SessionTimeout = DefaultSessionTimeout
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	hc, err := transport.New(transport.Options{
		Timeout:  opts.Timeout,
		ProxyURL: opts.ProxyURL,
		Logger:   opts.Logger,
	})
	if err != nil {
		return nil, err
	}

	svc := &Service{
		opts:     opts,
		hc:       hc,
		version:  version,
		logger:   opts.Logger,
		newTitle: defaultTitleFactory,
	}
	if !opts.DisableReuse {
		svc.cache = newResultCache(opts.ReuseTTL)
	}
	return svc, nil
}

// defaultTitleFactory 构造真实的机台客户端。
func defaultTitleFactory(opts protocol.TitleOptions) (titleSession, error) {
	return protocol.NewTitleClient(opts)
}

// Version 返回生效的协议版本号（探测成功后是探测到的版本）。
func (s *Service) Version() string {
	s.versionMu.RLock()
	defer s.versionMu.RUnlock()
	return s.version.Encoding
}

// currentVersion 返回当前生效的协议参数。
func (s *Service) currentVersion() protocol.Version {
	s.versionMu.RLock()
	defer s.versionMu.RUnlock()
	return s.version
}

// SessionResult 是一次取分会话的结果。
type SessionResult struct {
	// UserID 是机台账号。
	UserID int `json:"userId"`

	// Scores 是过滤掉未游玩条目后的全量成绩。
	Scores []model.Score `json:"-"`

	// Reused 报告本次结果是否来自同一二维码的复用缓存（未重新登录机台）。
	Reused bool `json:"reused"`

	// Warnings 记录不致命但需要让用户知道的问题，目前只有「登出未成功」。
	Warnings []string `json:"warnings,omitempty"`
}

// Fetch 完成一次完整会话：换账号、登录、取分、登出。
//
// 同一二维码在有效期内重复调用会直接返回上次的结果，不再登录机台：
// 二维码本身就是授权凭证，它还在有效期内时重复扫描得到的是同一份数据。
func (s *Service) Fetch(ctx context.Context, sgid protocol.SGID) (SessionResult, error) {
	if cached, ok := s.cache.loadScores(sgid); ok {
		if s.logger != nil {
			s.logger.Info("复用同一二维码的取分结果", "userId", cached.userID, "条数", len(cached.scores))
		}
		return SessionResult{UserID: cached.userID, Scores: cached.scores, Reused: true}, nil
	}

	var scores []model.Score
	cred, warnings, err := s.session(ctx, sgid,
		func(sessionCtx context.Context, client titleSession, _ protocol.Credential) error {
			got, err := client.Music(sessionCtx)
			if err != nil {
				return err
			}
			scores = got
			return nil
		})
	if err != nil {
		return SessionResult{Warnings: warnings}, err
	}
	s.cache.storeScores(sgid, cred.UserID, scores)
	return SessionResult{UserID: cred.UserID, Scores: scores, Warnings: warnings}, nil
}

// Profile 拉取账号资料：头像、姓名框、牌子、称号与评级。
//
// 与 Fetch 一样，同一二维码在有效期内重复调用会复用上次结果。
func (s *Service) Profile(ctx context.Context, sgid protocol.SGID) (protocol.Profile, error) {
	if cached, ok := s.cache.loadProfile(sgid); ok {
		if s.logger != nil {
			s.logger.Info("复用同一二维码的资料结果", "userId", cached.profile.UserID)
		}
		return cached.profile, nil
	}

	var profile protocol.Profile
	_, _, err := s.session(ctx, sgid,
		func(sessionCtx context.Context, client titleSession, _ protocol.Credential) error {
			got, err := client.Profile(sessionCtx)
			if err != nil {
				return err
			}
			profile = got
			return nil
		})
	if err != nil {
		return protocol.Profile{}, err
	}
	s.cache.storeProfile(sgid, profile)
	return profile, nil
}

// session 封装一次「换账号 → 登录 → 执行 → 登出」。
//
// 登出在 defer 里执行且不继承会话上下文：即使 fn 中途失败或会话超时，登出请求也会发出，
// 否则会话会残留到 15 分钟硬超时，期间该账号无法再次登录。
// 返回值用了具名参数，否则 defer 里追加的警告不会被带回调用方。
func (s *Service) session(
	ctx context.Context,
	sgid protocol.SGID,
	fn func(context.Context, titleSession, protocol.Credential) error,
) (cred protocol.Credential, warnings []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !sgid.Valid() {
		return cred, nil, fmt.Errorf("%w: 二维码未通过校验", protocol.ErrSGIDFormat)
	}
	if sgid.Expired(time.Now()) {
		return cred, nil, protocol.ErrSGIDExpired
	}

	cred, err = s.exchange(ctx, sgid)
	if err != nil {
		return cred, nil, err
	}

	client, err := s.newTitle(protocol.TitleOptions{
		HTTP:    s.hc,
		Version: s.currentVersion(),
		BaseURL: s.opts.TitleBaseURL,
		Logger:  s.logger,
	})
	if err != nil {
		return cred, nil, err
	}

	sessionCtx, cancel := context.WithTimeout(ctx, s.opts.SessionTimeout)
	defer cancel()

	if err := client.Login(sessionCtx, cred, 0); err != nil {
		return cred, nil, err
	}

	defer func() {
		if logoutErr := s.logout(client, ctx); logoutErr != nil {
			if s.logger != nil {
				s.logger.Error("登出失败，账号会话会残留到硬超时", "userId", cred.UserID, "原因", logoutErr.Error())
			}
			warnings = append(warnings,
				"登出未成功，账号会话可能在约 15 分钟内不可用: "+logoutErr.Error())
		}
	}()

	if err := fn(sessionCtx, client, cred); err != nil {
		return cred, warnings, err
	}
	return cred, warnings, nil
}

// logout 用独立上下文发起登出，保证会话超时或调用方取消后仍能尝试收尾。
func (s *Service) logout(client titleSession, parent context.Context) error {
	logoutCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), logoutTimeout)
	defer cancel()
	return client.Logout(logoutCtx)
}

// exchange 用二维码换取机台账号凭证。
func (s *Service) exchange(ctx context.Context, sgid protocol.SGID) (protocol.Credential, error) {
	client, err := protocol.NewAimeClient(protocol.AimeOptions{
		HTTP:   s.hc,
		URL:    s.opts.AimeURL,
		Logger: s.logger,
	})
	if err != nil {
		return protocol.Credential{}, err
	}
	return client.Exchange(ctx, sgid)
}

// ChartIndex 拉取曲目索引；同步与 b50 都需要它。
func (s *Service) ChartIndex(ctx context.Context) (*chart.Index, error) {
	cache := &chart.Cache{
		Dir:    s.opts.ChartCacheDir,
		TTL:    s.opts.ChartCacheTTL,
		Logger: s.logger,
	}
	loader, err := chart.NewLoader(chart.LoaderOptions{
		HTTP:    s.hc,
		BaseURL: s.opts.ChartBaseURL,
		Cache:   cache,
		Logger:  s.logger,
	})
	if err != nil {
		return nil, err
	}
	index, err := loader.Load(ctx)
	if err != nil {
		return nil, model.Errorf(model.KindNetwork, model.CodeSiteNoSongs, "",
			"拉取曲目索引失败", "确认能访问查分器的 music_data 接口", err)
	}
	return index, nil
}

// SyncRequest 是一次同步请求。
type SyncRequest struct {
	// SGID 是玩家二维码。
	SGID protocol.SGID

	// Site 是查分器注册名，为空取 divingfish。
	Site string

	// Credential 是目标查分器的凭证。
	Credential string

	// Scores 可选：调用方已取到成绩时直接复用，避免重复登录机台。
	Scores []model.Score
}

// SyncResult 是一次同步的结果统计。
type SyncResult struct {
	// UserID 是机台账号。
	UserID int `json:"userId"`

	// ScoreCount 是机台上报的成绩条数。
	ScoreCount int `json:"scoreCount"`

	// Site 是实际使用的查分器。
	Site string `json:"site"`

	// Warnings 透传会话期的非致命问题。
	Warnings []string `json:"warnings,omitempty"`
}

// Sync 取分后推送到指定查分器。
//
// 顺序：先校验站点名，再拉曲目索引，最后才碰机台会话。
// 参数写错不该产生任何网络请求——顺序反过来会让拼错的 --site 表现为网络故障，排查时会绕远路。
// 索引又先于会话：索引拉不到就没必要占用机台会话，能少一次登录就少一次。
func (s *Service) Sync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	siteName := req.Site
	if siteName == "" {
		siteName = "divingfish"
	}
	if !sitesync.Registered(siteName) {
		// 借 New 生成统一的错误信息（含可用取值），这里只保证它发生在任何网络请求之前。
		if _, err := sitesync.New(siteName, sitesync.Deps{}); err != nil {
			return SyncResult{}, err
		}
	}

	index, err := s.ChartIndex(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	scores := req.Scores
	userID := 0
	var warnings []string
	if len(scores) == 0 {
		session, err := s.Fetch(ctx, req.SGID)
		if err != nil {
			return SyncResult{}, err
		}
		scores, userID, warnings = session.Scores, session.UserID, session.Warnings
	}

	syncer, err := sitesync.New(siteName, sitesync.Deps{
		HTTP:    s.hc,
		Songs:   index,
		BaseURL: s.opts.ChartBaseURL,
		Logger:  s.logger,
	})
	if err != nil {
		return SyncResult{}, err
	}
	if err := syncer.Upload(ctx, scores, req.Credential); err != nil {
		return SyncResult{}, err
	}

	return SyncResult{
		UserID:     userID,
		ScoreCount: len(scores),
		Site:       syncer.Name(),
		Warnings:   warnings,
	}, nil
}
