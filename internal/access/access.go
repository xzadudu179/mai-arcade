// Package access 提供与接入方式无关的请求准入部件：鉴权、限频、体积与并发限额。
//
// 它不认识任何业务操作，HTTP 与 WebSocket 适配器都复用它，因此新增接入方式时
// 不必重写安全相关的逻辑。
package access

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// Subprotocol 是浏览器场景下承载凭证的 WebSocket 子协议前缀。
//
// 浏览器的 WebSocket 构造函数无法自定义请求头，因此除了 Authorization，
// 还需要一条能被子协议携带凭证的通道。
const Subprotocol = "mai-arcade.v1"

// 接入层具名错误码。
var (
	// ErrUnauthorized 表示凭证缺失或不匹配。
	ErrUnauthorized = &model.Error{
		Kind: model.KindParam, Sentinel: model.CodeUnauthorized,
		Msg:  "鉴权失败",
		Hint: "在 Authorization 头里带上 Bearer 令牌；确认服务端与客户端用的是同一个 --token",
	}

	// ErrRateLimited 表示触发了限频。限频保护的是出口 IP——它是全局稀缺资源。
	ErrRateLimited = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeRateLimited,
		Msg:  "请求过于频繁，已限频",
		Hint: "按 Retry-After 等待后重试；机台接口对频率敏感，触发封禁会影响整条线路",
	}

	// ErrTooLarge 表示请求体超过上限。
	ErrTooLarge = &model.Error{
		Kind: model.KindParam, Sentinel: model.CodeTooLarge,
		Msg:  "请求体过大",
		Hint: "本服务的请求只含少量参数；确认没有误传文件或大段文本",
	}

	// ErrBusy 表示并发已满。
	ErrBusy = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeBusy,
		Msg:  "服务繁忙，并发已满",
		Hint: "稍后重试；同一账号的机台会话本就必须串行，并发上限只是防堆积",
	}
)

func init() {
	for _, sentinel := range []*model.Error{ErrUnauthorized, ErrRateLimited, ErrTooLarge, ErrBusy} {
		model.RegisterCode(sentinel)
	}
}

// Authenticator 用单个共享令牌校验请求。
type Authenticator struct {
	token string
}

// NewAuthenticator 构造鉴权器；令牌为空视为配置错误，直接拒绝而不是放行。
func NewAuthenticator(token string) (*Authenticator, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("%w: 服务令牌不能为空，否则任何调用方都能用你的二维码与凭证",
			model.ErrParam)
	}
	if len(token) < minTokenLength {
		return nil, fmt.Errorf("%w: 服务令牌至少 %d 个字符（当前 %d）",
			model.ErrParam, minTokenLength, len(token))
	}
	return &Authenticator{token: token}, nil
}

// minTokenLength 是令牌最短长度：它保护的是一枚能读账号成绩并写入查分器的能力。
const minTokenLength = 16

// VerifyRequest 校验请求凭证，返回需要回显的 WebSocket 子协议（未使用则为空串）。
//
// 支持两种携带方式：Authorization: Bearer 与 Sec-WebSocket-Protocol 里的 bearer 形式。
// 比较使用常量时间实现，避免通过响应耗时逐字节猜出令牌。
func (a *Authenticator) VerifyRequest(r *http.Request) (string, error) {
	for _, candidate := range candidatesFrom(r) {
		if subtle.ConstantTimeCompare([]byte(candidate.token), []byte(a.token)) == 1 {
			// 回显与否取决于令牌是**从哪条通道**来的，而不是看头的前缀：
			// 浏览器可能一次提出多个子协议，此时仍必须回显我们选中的那一个。
			if candidate.viaSubprotocol {
				return Subprotocol, nil
			}
			return "", nil
		}
	}
	return "", ErrUnauthorized
}

// candidate 是一个候选凭证及其来源通道。
type candidate struct {
	token          string
	viaSubprotocol bool
}

// candidatesFrom 按优先级提取请求里的候选凭证。
func candidatesFrom(r *http.Request) []candidate {
	var out []candidate

	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := bearerToken(header); ok {
			out = append(out, candidate{token: token})
		}
	}
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		out = append(out, candidate{token: token})
	}
	// 浏览器的 WebSocket 无法自定义请求头，只能走子协议。
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, proto := range strings.Split(value, ",") {
			proto = strings.TrimSpace(proto)
			if token, ok := strings.CutPrefix(proto, Subprotocol+".bearer."); ok {
				out = append(out, candidate{token: token, viaSubprotocol: true})
			}
		}
	}
	return out
}

// bearerToken 从 Authorization 头里取出 Bearer 令牌，scheme 大小写不敏感。
func bearerToken(header string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}

// RateLimiter 是令牌桶限频器。
//
// 限频针对全局而非单个调用方：真正稀缺的是出口 IP，多客户端共享同一条线路，
// 所以按调用方分别限频挡不住整体频率过高。
type RateLimiter struct {
	mu     sync.Mutex
	burst  float64
	tokens float64
	perSec float64
	last   time.Time
	now    func() time.Time

	// retryAfter 记录上次被拒时需要等待的时长，供 Retry-After 头使用。
	retryAfter time.Duration
}

// NewRateLimiter 构造限频器：每 interval 允许 burst 次请求。
//
// burst 或 interval 非正时返回 nil，表示不限频。
func NewRateLimiter(burst int, interval time.Duration) *RateLimiter {
	if burst <= 0 || interval <= 0 {
		return nil
	}
	return &RateLimiter{
		burst:  float64(burst),
		tokens: float64(burst),
		perSec: float64(burst) / interval.Seconds(),
		now:    time.Now,
	}
}

// Allow 报告当前是否放行，并消耗一个令牌。
//
// 允许时 RetryAfter 无意义；被拒时可据此填 Retry-After 头。
func (l *RateLimiter) Allow() bool {
	if l == nil {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.last.IsZero() {
		l.last = now
	}
	// 漏桶按时间补齐，最多补到桶容量。
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.perSec)
	l.last = now

	if l.tokens >= 1 {
		l.tokens--
		l.retryAfter = 0
		return true
	}
	l.retryAfter = time.Duration((1 - l.tokens) / l.perSec * float64(time.Second))
	return false
}

// RetryAfter 返回建议的等待时长。
func (l *RateLimiter) RetryAfter() time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retryAfter
}

// Semaphore 限制同时在跑的操作数。
type Semaphore chan struct{}

// NewSemaphore 构造容量为 n 的信号量；n 非正时返回 nil，表示不限制。
func NewSemaphore(n int) Semaphore {
	if n <= 0 {
		return nil
	}
	return make(Semaphore, n)
}

// TryAcquire 尝试占用一个名额，不等待。
func (s Semaphore) TryAcquire() bool {
	if s == nil {
		return true
	}
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release 归还名额。
func (s Semaphore) Release() {
	if s == nil {
		return
	}
	select {
	case <-s:
	default:
	}
}

// DefaultMaxBody 是请求体默认上限：本服务的请求只含少量参数。
const DefaultMaxBody = 64 << 10

// ReadLimited 读取请求体，超过上限时返回分类好的错误。
//
// 与 http.MaxBytesReader 的区别是它返回 model.Error，便于统一映射成响应。
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取请求体: %v", model.ErrNetwork, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: 上限 %d 字节", ErrTooLarge, limit)
	}
	return body, nil
}

// IsEmptyBody 报告请求体是否为空。
func IsEmptyBody(body []byte) bool { return len(strings.TrimSpace(string(body))) == 0 }
