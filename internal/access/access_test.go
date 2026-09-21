package access

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNewAuthenticatorRejectsWeakTokens 断言过短或空的令牌让服务拒绝启动。
//
// 令牌保护的是一枚能读账号成绩、写入查分器的能力，弱令牌等于没有鉴权。
func TestNewAuthenticatorRejectsWeakTokens(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"空令牌", ""},
		{"只有空白", "   "},
		{"过短", "short"},
		{"差一个字符到下限", strings.Repeat("a", minTokenLength-1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAuthenticator(tc.token); err == nil {
				t.Errorf("令牌 %q 应当被拒绝", tc.token)
			}
		})
	}
}

// TestNewAuthenticatorAcceptsReasonableToken 断言长度达标的令牌被接受。
func TestNewAuthenticatorAcceptsReasonableToken(t *testing.T) {
	if _, err := NewAuthenticator(strings.Repeat("a", minTokenLength)); err != nil {
		t.Errorf("达标长度不应报错: %v", err)
	}
}

// newRequest 构造带指定头的请求。
func newRequest(headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/ops", nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req
}

// TestVerifyRequestAcceptsSupportedCarriers 断言两种凭证携带方式都被接受。
func TestVerifyRequestAcceptsSupportedCarriers(t *testing.T) {
	const token = "0123456789abcdef0123"
	auth, err := NewAuthenticator(token)
	if err != nil {
		t.Fatalf("构造鉴权器失败: %v", err)
	}

	tests := []struct {
		name            string
		headers         map[string]string
		wantSubprotocol string
	}{
		{"Authorization Bearer", map[string]string{"Authorization": "Bearer " + token}, ""},
		{"scheme 大小写不敏感", map[string]string{"Authorization": "bEaReR " + token}, ""},
		{"X-Auth-Token", map[string]string{"X-Auth-Token": token}, ""},
		{
			"WebSocket 子协议",
			map[string]string{"Sec-WebSocket-Protocol": Subprotocol + ".bearer." + token},
			Subprotocol,
		},
		{
			"子协议与被拒绝的协议混在一起",
			map[string]string{"Sec-WebSocket-Protocol": "other.protocol, " + Subprotocol + ".bearer." + token},
			Subprotocol,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subprotocol, err := auth.VerifyRequest(newRequest(tc.headers))
			if err != nil {
				t.Fatalf("应通过鉴权: %v", err)
			}
			if subprotocol != tc.wantSubprotocol {
				t.Errorf("子协议 = %q, 期望 %q", subprotocol, tc.wantSubprotocol)
			}
		})
	}
}

// TestVerifyRequestRejectsBadCredentials 断言凭证不对时返回未授权。
func TestVerifyRequestRejectsBadCredentials(t *testing.T) {
	const token = "0123456789abcdef0123"
	auth, err := NewAuthenticator(token)
	if err != nil {
		t.Fatalf("构造鉴权器失败: %v", err)
	}

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"没有凭证", nil},
		{"令牌不匹配", map[string]string{"Authorization": "Bearer wrong-token-here"}},
		{"令牌前缀正确但不完整", map[string]string{"Authorization": "Bearer " + token[:10]}},
		{"scheme 不对", map[string]string{"Authorization": "Basic " + token}},
		{"子协议里没有 bearer 形式", map[string]string{"Sec-WebSocket-Protocol": Subprotocol}},
		{"X-Auth-Token 为空", map[string]string{"Authorization": "Bearer nope"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.VerifyRequest(newRequest(tc.headers)); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("错误 = %v, 期望 %v", err, ErrUnauthorized)
			}
		})
	}
}

// TestRateLimiterAllowsBurstThenBlocks 断言令牌桶先放行突发、超过后拒绝。
func TestRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	limiter := NewRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !limiter.Allow() {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	if limiter.Allow() {
		t.Fatal("第 4 次应被拒绝")
	}
	if limiter.RetryAfter() <= 0 {
		t.Error("被拒后应给出等待时长")
	}
}

// TestRateLimiterRefillsOverTime 断言令牌随时间补齐，且不会超过桶容量。
func TestRateLimiterRefillsOverTime(t *testing.T) {
	limiter := NewRateLimiter(2, time.Minute) // 每 30 秒补一个

	now := time.Now()
	limiter.now = func() time.Time { return now }

	if !limiter.Allow() || !limiter.Allow() {
		t.Fatal("初始两次应放行")
	}
	if limiter.Allow() {
		t.Fatal("桶空后应拒绝")
	}

	// 过 30 秒补一个令牌。
	now = now.Add(30 * time.Second)
	if !limiter.Allow() {
		t.Error("补足一个令牌后应放行")
	}
	if limiter.Allow() {
		t.Error("只补了一个令牌，第二次仍应拒绝")
	}

	// 久置之后最多补满到桶容量。
	now = now.Add(time.Hour)
	allowed := 0
	for i := 0; i < 5; i++ {
		if limiter.Allow() {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("长时间空闲后放行 %d 次, 期望补满到容量 2", allowed)
	}
}

// TestNilRateLimiterAllowsEverything 断言不限频时永远放行。
func TestNilRateLimiterAllowsEverything(t *testing.T) {
	var limiter *RateLimiter
	for i := 0; i < 100; i++ {
		if !limiter.Allow() {
			t.Fatal("nil 限频器应永远放行")
		}
	}
	if limiter.RetryAfter() != 0 {
		t.Error("nil 限频器的等待时长应为 0")
	}

	if NewRateLimiter(0, time.Minute) != nil {
		t.Error("burst 为 0 应表示不限频")
	}
	if NewRateLimiter(5, 0) != nil {
		t.Error("窗口为 0 应表示不限频")
	}
}

// TestRateLimiterIsConcurrencySafe 断言并发调用不会超发。
//
// -race 同时检查这里的数据竞争。
func TestRateLimiterIsConcurrencySafe(t *testing.T) {
	limiter := NewRateLimiter(50, time.Hour)

	var (
		mu      sync.Mutex
		allowed int
		wg      sync.WaitGroup
	)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.Allow() {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != 50 {
		t.Errorf("放行 %d 次, 期望恰好 50（并发下不能超发）", allowed)
	}
}

// TestSemaphoreLimitsConcurrency 断言信号量限制并发数。
func TestSemaphoreLimitsConcurrency(t *testing.T) {
	sem := NewSemaphore(2)

	if !sem.TryAcquire() || !sem.TryAcquire() {
		t.Fatal("容量为 2 时前两次应成功")
	}
	if sem.TryAcquire() {
		t.Error("占满后应失败")
	}

	sem.Release()
	if !sem.TryAcquire() {
		t.Error("释放后应能再次占用")
	}
}

// TestNilSemaphoreAllowsEverything 断言不限制并发时 TryAcquire 永远成功。
func TestNilSemaphoreAllowsEverything(t *testing.T) {
	var sem Semaphore
	for i := 0; i < 100; i++ {
		if !sem.TryAcquire() {
			t.Fatal("nil 信号量应永远成功")
		}
	}
	sem.Release() // 不应 panic

	if NewSemaphore(0) != nil {
		t.Error("容量为 0 应表示不限制")
	}
}

// TestSemaphoreReleaseWithoutAcquireIsSafe 断言多余的 Release 不会破坏计数。
func TestSemaphoreReleaseWithoutAcquireIsSafe(t *testing.T) {
	sem := NewSemaphore(1)
	sem.Release()
	sem.Release()

	if !sem.TryAcquire() {
		t.Error("多余的 Release 不应吞掉可用名额")
	}
}

// TestReadLimited 断言请求体读取的上限行为。
func TestReadLimited(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		limit   int64
		wantErr error
	}{
		{"小于上限", "hello", 16, nil},
		{"恰好等于上限", strings.Repeat("a", 16), 16, nil},
		{"超过上限一位", strings.Repeat("a", 17), 16, ErrTooLarge},
		{"远大于上限", strings.Repeat("a", 4096), 16, ErrTooLarge},
		{"零值用默认上限", "hello", 0, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadLimited(strings.NewReader(tc.body), tc.limit)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("错误 = %v, 期望 %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("内容 = %q, 期望 %q", got, tc.body)
			}
		})
	}
}

// TestIsEmptyBody 断言空体判定忽略空白。
func TestIsEmptyBody(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"\n\t ", true},
		{"{}", false},
	}
	for _, tc := range tests {
		if got := IsEmptyBody([]byte(tc.body)); got != tc.want {
			t.Errorf("IsEmptyBody(%q) = %v, 期望 %v", tc.body, got, tc.want)
		}
	}
}
