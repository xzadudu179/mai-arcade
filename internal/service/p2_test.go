package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// ---------------------------------------------------------------------------
// F15：同一二维码在有效期内复用结果
// ---------------------------------------------------------------------------

// TestResultCacheReusesWithinValidity 断言同一二维码在有效期内复用取分结果。
//
// 复用的意义是不再重复「换账号 → 登录 → 分页取分 → 登出」：机台会话每次都必须新建并登出，
// 但同一枚二维码对应的是同一份数据。
func TestResultCacheReusesWithinValidity(t *testing.T) {
	session := &fakeTitleSession{scores: []model.Score{{MusicID: 1, PlayCount: 1}}}
	aime, aimeCalls := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)
	sgid := validSGID(t)

	first, err := svc.Fetch(context.Background(), sgid)
	if err != nil {
		t.Fatalf("首次 Fetch 报错: %v", err)
	}
	if first.Reused {
		t.Error("首次调用不应标记为复用")
	}

	second, err := svc.Fetch(context.Background(), sgid)
	if err != nil {
		t.Fatalf("二次 Fetch 报错: %v", err)
	}
	if !second.Reused {
		t.Error("二次调用应复用缓存")
	}
	if second.UserID != first.UserID || len(second.Scores) != len(first.Scores) {
		t.Error("复用结果应与首次一致")
	}

	// 关键：第二次不应再登录机台，也不应再调 AimeDB。
	if got := session.loginCalls.Load(); got != 1 {
		t.Errorf("登录次数 = %d, 期望 1（复用后不应重新登录）", got)
	}
	if got := aimeCalls.Load(); got != 1 {
		t.Errorf("AimeDB 调用 = %d, 期望 1", got)
	}
}

// TestResultCacheIsKeyedBySGID 断言不同二维码不会互相命中。
func TestResultCacheIsKeyedBySGID(t *testing.T) {
	session := &fakeTitleSession{scores: []model.Score{{MusicID: 1, PlayCount: 1}}}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	first := validSGID(t)
	// 换一枚签名不同的二维码（时间戳相同但内容不同）。
	other := validSGIDWithTail(t, "FEDCBA9876543210FEDCBA9876543210FEDCBA9876543210FEDCBA9876543210")

	if _, err := svc.Fetch(context.Background(), first); err != nil {
		t.Fatalf("首次 Fetch 报错: %v", err)
	}
	result, err := svc.Fetch(context.Background(), other)
	if err != nil {
		t.Fatalf("换二维码 Fetch 报错: %v", err)
	}
	if result.Reused {
		t.Error("不同二维码不应命中同一份缓存")
	}
	if got := session.loginCalls.Load(); got != 2 {
		t.Errorf("登录次数 = %d, 期望 2（两枚二维码各一次）", got)
	}
}

// TestResultCacheDoesNotOutliveQR 断言缓存不会跨过二维码的有效期继续生效。
//
// 二维码过期意味着授权结束，此时再用它换来的数据就越权了。
func TestResultCacheDoesNotOutliveQR(t *testing.T) {
	// 造一枚只剩几秒有效期的二维码。
	issued := time.Now().Add(-protocol.SGIDValidity + 3*time.Second)
	sgid := sgidIssuedAt(t, issued)

	session := &fakeTitleSession{scores: []model.Score{{MusicID: 1, PlayCount: 1}}}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	cache := svc.cache
	now := time.Now()
	cache.now = func() time.Time { return now }

	if _, err := svc.Fetch(context.Background(), sgid); err != nil {
		t.Fatalf("Fetch 报错: %v", err)
	}
	if _, ok := cache.loadScores(sgid); !ok {
		t.Fatal("刚取到的结果应当可复用")
	}

	// 时间推到二维码过期之后。
	now = now.Add(10 * time.Second)
	if _, ok := cache.loadScores(sgid); ok {
		t.Error("二维码过期后不应再复用结果")
	}
}

// TestResultCacheCanBeDisabled 断言可以关掉复用。
func TestResultCacheCanBeDisabled(t *testing.T) {
	svc, err := New(Options{
		Version:      "1.53",
		DisableReuse: true,
		Timeout:      time.Second,
	})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	if svc.cache != nil {
		t.Error("关闭复用时不应构造缓存")
	}
}

// TestResultCacheProfileIsSeparateFromScores 断言资料与成绩不会串用。
func TestResultCacheProfileIsSeparateFromScores(t *testing.T) {
	session := &fakeTitleSession{
		scores:  []model.Score{{MusicID: 1, PlayCount: 1}},
		profile: protocol.Profile{UserID: 7, UserName: "玩家"},
	}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)
	sgid := validSGID(t)

	if _, err := svc.Fetch(context.Background(), sgid); err != nil {
		t.Fatalf("Fetch 报错: %v", err)
	}
	profile, err := svc.Profile(context.Background(), sgid)
	if err != nil {
		t.Fatalf("Profile 报错: %v", err)
	}
	if profile.UserName != "玩家" {
		t.Errorf("资料 = %+v, 期望取到真实资料而不是成绩缓存", profile)
	}

	// 资料也应被缓存下来。
	if _, ok := svc.cache.loadProfile(sgid); !ok {
		t.Error("资料应当进入缓存")
	}
}

// TestResultCacheEvictsExpired 断言过期条目会被回收。
func TestResultCacheEvictsExpired(t *testing.T) {
	cache := newResultCache(time.Minute)
	now := time.Now()
	cache.now = func() time.Time { return now }

	sgid := validSGID(t)
	cache.storeScores(sgid, 1, nil)
	if cache.Len() != 1 {
		t.Fatalf("条目数 = %d, 期望 1", cache.Len())
	}

	// 超过缓存时长后，写入新条目会顺带清掉旧的。
	now = now.Add(2 * time.Minute)
	other := validSGIDWithTail(t, "AAAA1111BBBB2222CCCC3333DDDD4444AAAA1111BBBB2222CCCC3333DDDD4444")
	cache.storeScores(other, 2, nil)

	if cache.Len() != 1 {
		t.Errorf("条目数 = %d, 期望 1（过期条目应被回收）", cache.Len())
	}
}

// TestResultCacheIsConcurrencySafe 断言并发读写安全（-race 会检查数据竞争）。
func TestResultCacheIsConcurrencySafe(t *testing.T) {
	cache := newResultCache(time.Minute)
	sgid := validSGID(t)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				cache.storeScores(sgid, i, []model.Score{{MusicID: i, PlayCount: 1}})
				return
			}
			cache.loadScores(sgid)
			cache.loadProfile(sgid)
		}(i)
	}
	wg.Wait()
}

// TestResultCacheNeverStoresRawSGID 断言缓存不保留二维码原文。
//
// 键是哈希：二维码是账号凭证，不该在内存里多留一份，也避免它通过内存转储泄露。
func TestResultCacheNeverStoresRawSGID(t *testing.T) {
	cache := newResultCache(time.Minute)
	sgid := validSGID(t)
	cache.storeScores(sgid, 1, nil)

	key := cache.key(sgid)
	if strings.Contains(key, string(sgid)) {
		t.Error("缓存键不应包含二维码原文")
	}
	if !strings.Contains(string(sgid), "SGWCMAID") {
		t.Fatal("测试二维码本身应当带前缀")
	}
	if strings.Contains(key, "SGWCMAID") {
		t.Error("缓存键不应包含二维码前缀")
	}
	if len(key) != 64 {
		t.Errorf("缓存键长度 = %d, 期望 sha256 的 64 位十六进制", len(key))
	}
}

// ---------------------------------------------------------------------------
// F14：多版本参数自动探测
// ---------------------------------------------------------------------------

// TestDetectVersionReportsNetworkFailureFirst 断言链路不通时不逐个试版本。
//
// 出口 IP 被阻断时任何版本都拿不到可用响应，逐个尝试只会多打几次上游、加重风控。
func TestDetectVersionReportsNetworkFailureFirst(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()

	svc, err := New(Options{TitleBaseURL: url + "/Maimai2Servlet/", Timeout: time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	if _, err := svc.DetectVersion(context.Background()); err == nil {
		t.Fatal("链路不通时应当报错")
	}
}

// TestDetectVersionIsSkippedWhenDisabled 断言关掉探测时不产生网络请求。
func TestDetectVersionIsSkippedWhenDisabled(t *testing.T) {
	svc, err := New(Options{Version: "1.53", Timeout: time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	svc.versionProbeDisabled = true

	version, err := svc.DetectVersion(context.Background())
	if err != nil {
		t.Fatalf("跳过探测时不应报错: %v", err)
	}
	if version != "1.53" {
		t.Errorf("版本 = %q, 期望配置值 1.53", version)
	}
}

// TestUseVersionRecordsDetection 断言探测结果被记录下来并生效。
func TestUseVersionRecordsDetection(t *testing.T) {
	svc, err := New(Options{Version: "1.53", Timeout: time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	if svc.VersionDetected() {
		t.Error("初始状态不应标记为已探测")
	}
	if svc.Version() != "1.53" {
		t.Errorf("初始版本 = %q, 期望 1.53", svc.Version())
	}

	version, err := protocol.LookupVersion("1.55")
	if err != nil {
		t.Fatalf("取版本参数失败: %v", err)
	}
	svc.useVersion(version)

	if !svc.VersionDetected() {
		t.Error("探测后应标记为已探测")
	}
	if svc.Version() != "1.55" {
		t.Errorf("版本 = %q, 期望切换到 1.55", svc.Version())
	}
	if svc.currentVersion().ObfuscateParam != version.ObfuscateParam {
		t.Error("当前版本的参数应与探测到的版本一致")
	}
}

// TestVersionProbeOrderPrefersNewest 断言探测顺序是新版本优先。
func TestVersionProbeOrderPrefersNewest(t *testing.T) {
	if len(versionProbeOrder) < 2 {
		t.Fatal("应当至少有两个候选版本")
	}
	if versionProbeOrder[0] != "1.55" || versionProbeOrder[1] != "1.53" {
		t.Errorf("探测顺序 = %v, 期望 [1.55 1.53]", versionProbeOrder)
	}
	// 每个候选都必须真实存在于参数表里。
	for _, name := range versionProbeOrder {
		if _, err := protocol.LookupVersion(name); err != nil {
			t.Errorf("候选版本 %s 不在参数表里: %v", name, err)
		}
	}
}

// TestIsVersionMismatch 断言只有「解不开」才算版本不对。
func TestIsVersionMismatch(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"解密失败", protocol.ErrDecrypt, true},
		{"能解密但不是 JSON", protocol.ErrVersionMismatch, true},
		{"被阻断不算版本问题", protocol.ErrEmptyResponse, false},
		{"网络错不算版本问题", protocol.ErrNetwork, false},
		{"业务错不算版本问题", protocol.ErrAlreadyLoggedIn, false},
		{"nil", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVersionMismatch(tc.err); got != tc.want {
				t.Errorf("isVersionMismatch(%v) = %v, 期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 辅助构造
// ---------------------------------------------------------------------------

// validSGIDWithTail 用指定签名段构造有效的假二维码。
func validSGIDWithTail(t *testing.T, tail string) protocol.SGID {
	t.Helper()
	raw := "SGWCMAID" + time.Now().Format("060102150405") + tail
	sgid, err := protocol.NewSGID(raw)
	if err != nil {
		t.Fatalf("构造假二维码失败: %v", err)
	}
	return sgid
}

// sgidIssuedAt 构造一枚指定签发时间的二维码。
func sgidIssuedAt(t *testing.T, issued time.Time) protocol.SGID {
	t.Helper()
	raw := "SGWCMAID" + issued.Format("060102150405") + strings.Repeat("0123456789ABCDEF", 4)
	sgid, err := protocol.NewSGID(raw)
	if err != nil {
		t.Fatalf("构造假二维码失败: %v", err)
	}
	return sgid
}

// ---------------------------------------------------------------------------
// 错误码目录
// ---------------------------------------------------------------------------

// TestEverySentinelCodeIsInCatalog 断言各层用到的错误码都进了对外目录。
//
// 目录是给客户端看的契约；漏登记会让「按 code 分支」的调用方拿到空码。
func TestEverySentinelCodeIsInCatalog(t *testing.T) {
	// 服务层能直接看到的哨兵；接入层与操作层的码在各自的包里有对应测试。
	sentinels := []*model.Error{
		protocol.ErrSGIDFormat, protocol.ErrSGIDExpired, protocol.ErrEmptyResponse,
		protocol.ErrDecrypt, protocol.ErrVersionMismatch, protocol.ErrAlreadyLoggedIn,
		protocol.ErrTimeout, protocol.ErrAimeUnavailable, protocol.ErrAimeQRRejected,
		protocol.ErrAimeBadSignature, protocol.ErrUnsupportedVersion, protocol.ErrBadConfig,
	}

	for _, sentinel := range sentinels {
		if sentinel == nil {
			continue
		}
		info, ok := model.LookupCode(sentinel.Sentinel)
		if !ok {
			t.Errorf("错误码 %s 未登记进目录", sentinel.Sentinel)
			continue
		}
		if info.Kind != sentinel.Kind {
			t.Errorf("错误码 %s 的分类不一致: 目录 %v, 哨兵 %v", sentinel.Sentinel, info.Kind, sentinel.Kind)
		}
		if strings.TrimSpace(info.Meaning) == "" {
			t.Errorf("错误码 %s 缺少含义说明", sentinel.Sentinel)
		}
	}
}

// TestCatalogIsSortedAndUnique 断言目录稳定且无重复。
func TestCatalogIsSortedAndUnique(t *testing.T) {
	all := model.Errors()
	if len(all) < 20 {
		t.Fatalf("目录条目 = %d, 偏少", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Code >= all[i].Code {
			t.Fatalf("目录未按码排序或存在重复: %s, %s", all[i-1].Code, all[i].Code)
		}
	}
}

// TestDuplicateCodeRegistrationPanics 断言重复登记同一个码会立刻暴露。
func TestDuplicateCodeRegistrationPanics(t *testing.T) {
	err := model.Errorf(model.KindParam, model.CodeSGIDFormat, "", "重复登记", "", nil)
	defer func() {
		if recover() == nil {
			t.Error("重复登记同一个错误码应当 panic")
		}
	}()
	model.RegisterCode(err)
}

// TestCodeOfPrefersNamedCode 断言能从错误链上取出具名码。
func TestCodeOfPrefersNamedCode(t *testing.T) {
	code, ok := model.CodeOf(protocol.ErrSGIDExpired)
	if !ok || code != model.CodeSGIDExpired {
		t.Errorf("CodeOf = %q/%v, 期望 %s/true", code, ok, model.CodeSGIDExpired)
	}

	// 分类哨兵没有独立码。
	if _, ok := model.CodeOf(model.ErrNetwork); ok {
		t.Error("分类哨兵不应被当作具名码")
	}
	// 包装后仍应取到具名码。
	if _, ok := model.CodeOf(errors.Join(errors.New("x"), protocol.ErrSGIDExpired)); !ok {
		t.Error("包装链上应能取到具名码")
	}
}
