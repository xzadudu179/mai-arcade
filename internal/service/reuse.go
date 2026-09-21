package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// 会话结果复用（F15）。
//
// 复用的是**结果**而不是机台会话：机台会话用完必须登出，否则账号会进 15 分钟「小黑屋」，
// 所以重复调用不可能共用同一条登录态。能省掉的是「扫码换账号 → 登录 → 分页取分 → 登出」
// 这一整轮动作——同一枚二维码在有效期内再次被使用时，直接返回上次的结果。
//
// 键是二维码的哈希而不是原文：原文是账号凭证，不该在内存里多留一份，
// 哈希也避免了它通过内存转储或调试输出泄露。

// defaultReuseTTL 是结果复用的时间上限。
//
// 它同时受二维码剩余有效期约束（见 remainingValidity），因此不会超过二维码自身的授权窗口。
const defaultReuseTTL = 5 * time.Minute

// cacheKind 区分缓存的是哪一类结果，避免把资料当成成绩返回。
type cacheKind string

const (
	cacheKindScores  cacheKind = "scores"
	cacheKindProfile cacheKind = "profile"
)

// cachedScores 是缓存的取分结果。
type cachedScores struct {
	userID  int
	scores  []model.Score
	expires time.Time
}

// cachedProfile 是缓存的资料结果。
type cachedProfile struct {
	profile protocol.Profile
	expires time.Time
}

// resultCache 是按二维码哈希索引的内存缓存。
type resultCache struct {
	mu      sync.Mutex
	scores  map[string]cachedScores
	profile map[string]cachedProfile
	ttl     time.Duration
	now     func() time.Time
}

// newResultCache 构造缓存；ttl 非正时取 defaultReuseTTL。
func newResultCache(ttl time.Duration) *resultCache {
	if ttl <= 0 {
		ttl = defaultReuseTTL
	}
	return &resultCache{
		scores:  make(map[string]cachedScores),
		profile: make(map[string]cachedProfile),
		ttl:     ttl,
		now:     time.Now,
	}
}

// key 由二维码算出缓存键。
func (c *resultCache) key(sgid protocol.SGID) string {
	sum := sha256.Sum256([]byte(sgid))
	return hex.EncodeToString(sum[:])
}

// expiry 计算条目过期时间：取「缓存时长」与「二维码剩余有效期」中较早的一个。
//
// 二维码一旦过期，它的授权就结束了，此时绝不能再用它换来的数据。
func (c *resultCache) expiry(sgid protocol.SGID, now time.Time) time.Time {
	expiry := now.Add(c.ttl)
	if issuedAt, err := sgid.IssuedAt(); err == nil {
		if qrDeadline := issuedAt.Add(protocol.SGIDValidity); qrDeadline.Before(expiry) {
			expiry = qrDeadline
		}
	}
	return expiry
}

// loadScores 取出仍有效的成绩缓存。
func (c *resultCache) loadScores(sgid protocol.SGID) (cachedScores, bool) {
	if c == nil {
		return cachedScores{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.scores[c.key(sgid)]
	if !ok || !c.now().Before(entry.expires) {
		return cachedScores{}, false
	}
	return entry, true
}

// storeScores 写入成绩缓存。
func (c *resultCache) storeScores(sgid protocol.SGID, userID int, scores []model.Score) {
	if c == nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.evictLocked(now)
	c.scores[c.key(sgid)] = cachedScores{
		userID:  userID,
		scores:  scores,
		expires: c.expiry(sgid, now),
	}
}

// loadProfile 取出仍有效的资料缓存。
func (c *resultCache) loadProfile(sgid protocol.SGID) (cachedProfile, bool) {
	if c == nil {
		return cachedProfile{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.profile[c.key(sgid)]
	if !ok || !c.now().Before(entry.expires) {
		return cachedProfile{}, false
	}
	return entry, true
}

// storeProfile 写入资料缓存。
func (c *resultCache) storeProfile(sgid protocol.SGID, profile protocol.Profile) {
	if c == nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.evictLocked(now)
	c.profile[c.key(sgid)] = cachedProfile{profile: profile, expires: c.expiry(sgid, now)}
}

// evictLocked 清掉已过期的条目。
//
// 条目数量天然受「有效二维码个数」限制（二维码 10 分钟过期且量少），
// 但长时间运行的服务仍应主动回收，避免只增不减。
func (c *resultCache) evictLocked(now time.Time) {
	for key, entry := range c.scores {
		if !now.Before(entry.expires) {
			delete(c.scores, key)
		}
	}
	for key, entry := range c.profile {
		if !now.Before(entry.expires) {
			delete(c.profile, key)
		}
	}
}

// Len 返回当前缓存条目数，供测试与观测使用。
func (c *resultCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.scores) + len(c.profile)
}
