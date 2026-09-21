package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// ---------------------------------------------------------------------------
// 四类故障判定：这些是纯函数，直接喂错误值就能覆盖全部分支。
// ---------------------------------------------------------------------------

// TestTitleVerdictClassification 断言标题服务器的四类故障判定。
//
// probe 是排查部署问题的唯一手段，判定错一格就会把人引到错误的方向。
func TestTitleVerdictClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass FaultClass
		wantOK    bool
		wantExit  int
	}{
		{
			name:      "成功",
			err:       nil,
			wantClass: ClassOK, wantOK: true, wantExit: 0,
		},
		{
			name:      "响应 0 字节判为被阻断",
			err:       protocol.ErrEmptyResponse,
			wantClass: ClassBlocked, wantOK: false, wantExit: 2,
		},
		{
			name:      "解密失败判为参数版本不匹配",
			err:       protocol.ErrDecrypt,
			wantClass: ClassParams, wantOK: false, wantExit: 3,
		},
		{
			name:      "能解密但不是 JSON 同样判为版本不匹配",
			err:       protocol.ErrVersionMismatch,
			wantClass: ClassParams, wantOK: false, wantExit: 3,
		},
		{
			name:      "建连失败判为网络不通",
			err:       protocol.ErrNetwork,
			wantClass: ClassNetwork, wantOK: false, wantExit: 2,
		},
		{
			name:      "超时归入网络类",
			err:       protocol.ErrTimeout,
			wantClass: ClassNetwork, wantOK: false, wantExit: 2,
		},
		{
			name:      "业务返回码归为业务问题",
			err:       protocol.ErrAlreadyLoggedIn,
			wantClass: ClassBusiness, wantOK: false, wantExit: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := titleVerdictFromError(tc.err)
			if got.Class != tc.wantClass {
				t.Errorf("Class = %q, 期望 %q（%s）", got.Class, tc.wantClass, got.Detail)
			}
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, 期望 %v", got.OK, tc.wantOK)
			}
			if got.Class.exitCode() != tc.wantExit {
				t.Errorf("退出码 = %d, 期望 %d", got.Class.exitCode(), tc.wantExit)
			}
			if got.Target != "title" {
				t.Errorf("Target = %q, 期望 title", got.Target)
			}
		})
	}
}

// TestTitleBlockedVerdictExplainsRemedy 断言被阻断时给出可执行的补救建议。
//
// 只报一个 code 而不说下一步怎么办，等于把排查成本全丢给使用者。
func TestTitleBlockedVerdictExplainsRemedy(t *testing.T) {
	got := titleVerdictFromError(protocol.ErrEmptyResponse)

	// 三种成因与各自的处置都要提到：只给一条路会把使用者引向成本最高的那种做法。
	for _, want := range []string{"--version", "封禁", "IP", "48–72"} {
		if !strings.Contains(got.Hint, want) {
			t.Errorf("提示应包含 %q: %s", want, got.Hint)
		}
	}
}

// TestTitleParamsVerdictSuggestsVersionSwitch 断言参数不匹配时提示换版本。
func TestTitleParamsVerdictSuggestsVersionSwitch(t *testing.T) {
	got := titleVerdictFromError(protocol.ErrDecrypt)
	if !strings.Contains(got.Hint, "--version") {
		t.Errorf("提示应指导用户换协议版本: %s", got.Hint)
	}
}

// TestAimeVerdictClassification 断言 AimeDB 自检的判定。
func TestAimeVerdictClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass FaultClass
		wantOK    bool
	}{
		{
			name:      "换到账号说明一切正常",
			err:       nil,
			wantClass: ClassOK, wantOK: true,
		},
		{
			// 合成二维码必然查不到账号，服务端回 不可用 恰恰证明签名被接受了。
			name:      "二维码被拒说明请求格式与签名已被接受",
			err:       protocol.ErrAimeQRRejected,
			wantClass: ClassOK, wantOK: true,
		},
		{
			name:      "签名被拒是参数问题",
			err:       protocol.ErrAimeBadSignature,
			wantClass: ClassParams, wantOK: false,
		},
		{
			name:      "连不上是网络问题",
			err:       protocol.ErrAimeUnavailable,
			wantClass: ClassNetwork, wantOK: false,
		},
		{
			name:      "服务端业务码归为业务问题",
			err:       protocol.ErrBusiness,
			wantClass: ClassBusiness, wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := aimeVerdictFromError(tc.err)
			if got.Class != tc.wantClass {
				t.Errorf("Class = %q, 期望 %q（%s）", got.Class, tc.wantClass, got.Detail)
			}
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, 期望 %v", got.OK, tc.wantOK)
			}
		})
	}
}

// TestCombineTakesWorstClass 断言综合结论取两个目标里更严重的类别。
func TestCombineTakesWorstClass(t *testing.T) {
	tests := []struct {
		name string
		a, b FaultClass
		want FaultClass
	}{
		{"都通过才算通过", ClassOK, ClassOK, ClassOK},
		{"参数错优先于阻断", ClassBlocked, ClassParams, ClassParams},
		{"阻断优先于业务错", ClassBusiness, ClassBlocked, ClassBlocked},
		{"网络与阻断同级", ClassNetwork, ClassBlocked, ClassNetwork},
		{"任一失败即不通过", ClassOK, ClassBusiness, ClassBusiness},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := combine(tc.a, tc.b); got != tc.want {
				t.Errorf("combine(%q, %q) = %q, 期望 %q", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestSyntheticSGIDIsFormatValidButNotReal 断言自检用的合成二维码格式合法。
//
// 格式合法才能保证自检真的检验了时间戳格式与签名算法，而不是在本地就被拦下。
func TestSyntheticSGIDIsFormatValidButNotReal(t *testing.T) {
	now := time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)
	sgid, err := syntheticSGID(now)
	if err != nil {
		t.Fatalf("构造合成二维码失败: %v", err)
	}
	if !sgid.Valid() {
		t.Errorf("合成二维码应当格式合法: %s", sgid)
	}
	if sgid.Expired(now) {
		t.Error("合成二维码应当处于有效期内")
	}
	if len(string(sgid)) != 84 {
		t.Errorf("长度 = %d, 期望 84", len(string(sgid)))
	}
	// 签名为全零占位符，不对应任何真实账号。
	if !strings.HasSuffix(string(sgid), strings.Repeat("0", 64)) {
		t.Error("合成二维码的签名段应是占位符")
	}
}

// ---------------------------------------------------------------------------
// probe 端到端：用假服务器覆盖「被阻断」这条本机就能复现的路径。
// ---------------------------------------------------------------------------

// TestProbeReportsBlockedAndExitCode 断言 0 字节响应的机台被判为阻断并给出退出码 2。
func TestProbeReportsBlockedAndExitCode(t *testing.T) {
	title := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 复现被阻断的特征：HTTP 200 且响应体为空。
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(title.Close)

	aime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorID":1}`))
	}))
	t.Cleanup(aime.Close)

	svc, err := New(Options{TitleBaseURL: title.URL + "/Maimai2Servlet/", AimeURL: aime.URL})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	result, err := svc.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe 报错: %v", err)
	}

	verdicts := map[string]TargetVerdict{}
	for _, v := range result.Verdicts {
		verdicts[v.Target] = v
	}

	if got := verdicts["title"].Class; got != ClassBlocked {
		t.Errorf("机台判定 = %q, 期望 %q", got, ClassBlocked)
	}
	if got := verdicts["aime"].Class; got != ClassOK {
		t.Errorf("AimeDB 判定 = %q, 期望 %q（服务端回 errorID=1 说明签名被接受）", got, ClassOK)
	}
	if result.ExitCode != expectExit(ClassBlocked) {
		t.Errorf("综合退出码 = %d, 期望 %d", result.ExitCode, expectExit(ClassBlocked))
	}
	if result.Version == "" {
		t.Error("结果应带上使用的协议版本")
	}
}

// expectExit 让断言里的退出码取值可读。
func expectExit(c FaultClass) int { return c.exitCode() }

// TestProbeReportsNetworkFailure 断言连不上时判为网络不通。
func TestProbeReportsNetworkFailure(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()

	svc, err := New(Options{TitleBaseURL: url + "/Maimai2Servlet/", AimeURL: url})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	result, err := svc.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe 报错: %v", err)
	}
	if result.ExitCode != expectExit(ClassNetwork) {
		t.Errorf("综合退出码 = %d, 期望 %d", result.ExitCode, expectExit(ClassNetwork))
	}
	for _, v := range result.Verdicts {
		if v.Class != ClassNetwork {
			t.Errorf("目标 %s 判定 = %q, 期望 %q", v.Target, v.Class, ClassNetwork)
		}
	}
}

// TestProbeNoAimeErrorIDExposedAsFailure 断言 AimeDB 返回签名错误时判为参数问题。
func TestProbeAimeBadSignatureIsParams(t *testing.T) {
	title := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(title.Close)

	aime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorID":50}`))
	}))
	t.Cleanup(aime.Close)

	svc, err := New(Options{TitleBaseURL: title.URL + "/Maimai2Servlet/", AimeURL: aime.URL})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	result, err := svc.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe 报错: %v", err)
	}

	for _, v := range result.Verdicts {
		if v.Target != "aime" {
			continue
		}
		if v.Class != ClassParams {
			t.Errorf("AimeDB 判定 = %q, 期望 %q", v.Class, ClassParams)
		}
		if !strings.Contains(v.Detail, "50") {
			t.Errorf("应带上原始错误码: %s", v.Detail)
		}
	}
}

// ---------------------------------------------------------------------------
// 会话编排：登出必须发生，这是 §9 的验收项之一。
// ---------------------------------------------------------------------------

// fakeTitleSession 记录登录/登出/取分调用，用来观察编排行为。
type fakeTitleSession struct {
	loginErr  error
	logoutErr error
	scores    []model.Score
	musicErr  error
	profile   protocol.Profile

	// onMusic 在取分被调用时触发，用于制造「取分阶段父上下文被取消」这类时序。
	onMusic func()

	loginCalls  atomic.Int64
	logoutCalls atomic.Int64
	musicCalls  atomic.Int64
}

func (f *fakeTitleSession) Login(context.Context, protocol.Credential, int) error {
	f.loginCalls.Add(1)
	return f.loginErr
}

func (f *fakeTitleSession) Logout(context.Context) error {
	f.logoutCalls.Add(1)
	return f.logoutErr
}

func (f *fakeTitleSession) Music(context.Context) ([]model.Score, error) {
	f.musicCalls.Add(1)
	if f.onMusic != nil {
		f.onMusic()
	}
	return f.scores, f.musicErr
}

func (f *fakeTitleSession) Profile(context.Context) (protocol.Profile, error) {
	return f.profile, nil
}

// newTestService 构造一个机台与 AimeDB 都被替换掉的 Service。
func newTestService(t *testing.T, session *fakeTitleSession, aimeURL string) *Service {
	t.Helper()

	svc, err := New(Options{
		AimeURL: aimeURL,
		Version: protocol.DefaultVersion,
		Timeout: 5 * time.Second,
		// 会话超时设短一些，避免任何一条路径卡住整轮测试。
		SessionTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	svc.newTitle = func(protocol.TitleOptions) (titleSession, error) { return session, nil }
	return svc
}

// fakeAimeServer 返回一个稳定的假 AimeDB，并记录被调用次数。
func fakeAimeServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"errorID":0,"userID":10807675,"token":"token-abc"}`))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// validSGID 构造一枚当前有效的假二维码。
func validSGID(t *testing.T) protocol.SGID {
	t.Helper()
	raw := "SGWCMAID" + time.Now().Format("060102150405") + strings.Repeat("0123456789ABCDEF", 4)
	sgid, err := protocol.NewSGID(raw)
	if err != nil {
		t.Fatalf("构造假二维码失败: %v", err)
	}
	return sgid
}

// TestFetchLogsOutOnSuccess 断言正常路径下确实登出。
func TestFetchLogsOutOnSuccess(t *testing.T) {
	session := &fakeTitleSession{scores: []model.Score{{MusicID: 1, PlayCount: 1}}}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	result, err := svc.Fetch(context.Background(), validSGID(t))
	if err != nil {
		t.Fatalf("Fetch 报错: %v", err)
	}
	if session.loginCalls.Load() != 1 || session.logoutCalls.Load() != 1 {
		t.Errorf("登录/登出次数 = %d/%d, 期望 1/1", session.loginCalls.Load(), session.logoutCalls.Load())
	}
	if result.UserID != 10807675 {
		t.Errorf("UserID = %d, 期望 10807675", result.UserID)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("正常路径不应有警告: %v", result.Warnings)
	}
}

// TestFetchLogsOutWhenMusicFails 断言取分失败时仍然登出。
//
// 这是 §9 的验收项：拉取中途失败也必须收尾，否则账号会话残留到 15 分钟硬超时，
// 期间用户再也登不上机台。
func TestFetchLogsOutWhenMusicFails(t *testing.T) {
	session := &fakeTitleSession{musicErr: protocol.ErrNetwork}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	_, err := svc.Fetch(context.Background(), validSGID(t))
	if err == nil {
		t.Fatal("取分失败时 Fetch 应当报错")
	}
	if session.logoutCalls.Load() != 1 {
		t.Errorf("登出次数 = %d, 期望 1（异常路径也必须登出）", session.logoutCalls.Load())
	}
}

// TestFetchLogsOutWhenContextCancelled 断言调用方在取分阶段取消后仍尝试登出。
//
// 登出使用不继承会话上下文的独立上下文：否则父上下文一取消，登出请求就发不出去，
// 会话会一直残留到 15 分钟硬超时。
func TestFetchLogsOutWhenContextCancelled(t *testing.T) {
	session := &fakeTitleSession{musicErr: context.Canceled}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 在取分阶段取消：此时登录已经完成，正是需要保证登出发生的那一刻。
	session.onMusic = cancel

	if _, err := svc.Fetch(ctx, validSGID(t)); err == nil {
		t.Fatal("取分失败时 Fetch 应当报错")
	}

	if session.loginCalls.Load() != 1 {
		t.Fatalf("登录次数 = %d, 期望 1", session.loginCalls.Load())
	}
	if session.logoutCalls.Load() != 1 {
		t.Errorf("登出次数 = %d, 期望 1（父上下文取消也必须登出）", session.logoutCalls.Load())
	}
}

// TestFetchWarnsWhenLogoutFails 断言登出失败会作为警告带回，而不是被吞掉。
func TestFetchWarnsWhenLogoutFails(t *testing.T) {
	session := &fakeTitleSession{
		scores:    []model.Score{{MusicID: 1, PlayCount: 1}},
		logoutErr: protocol.ErrNetwork,
	}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	result, err := svc.Fetch(context.Background(), validSGID(t))
	if err != nil {
		t.Fatalf("登出失败不应让取分结果作废: %v", err)
	}
	if len(result.Scores) != 1 {
		t.Error("已经拿到的成绩应当保留")
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("警告数 = %d, 期望 1", len(result.Warnings))
	}
	if !strings.Contains(result.Warnings[0], "15") {
		t.Errorf("警告应说明会话会残留多久: %s", result.Warnings[0])
	}
}

// TestFetchDoesNotLogoutWhenLoginFails 断言登录失败时不发登出。
func TestFetchDoesNotLogoutWhenLoginFails(t *testing.T) {
	session := &fakeTitleSession{loginErr: protocol.ErrAlreadyLoggedIn}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	_, err := svc.Fetch(context.Background(), validSGID(t))
	if !errors.Is(err, protocol.ErrAlreadyLoggedIn) {
		t.Fatalf("错误 = %v, 期望已登录错误", err)
	}
	if session.logoutCalls.Load() != 0 {
		t.Errorf("登录失败时不应登出，实际 %d 次", session.logoutCalls.Load())
	}
}

// TestFetchRejectsBadSGIDBeforeTouchingNetwork 断言非法或过期二维码在本地被拦下。
func TestFetchRejectsBadSGIDBeforeTouchingNetwork(t *testing.T) {
	tests := []struct {
		name string
		sgid func(t *testing.T) protocol.SGID
		want error
	}{
		{
			name: "格式非法",
			sgid: func(*testing.T) protocol.SGID { return protocol.SGID("garbage") },
			want: protocol.ErrSGIDFormat,
		},
		{
			name: "已过期",
			sgid: func(t *testing.T) protocol.SGID {
				raw := "SGWCMAID" + time.Now().Add(-time.Hour).Format("060102150405") + strings.Repeat("0", 64)
				s, err := protocol.NewSGID(raw)
				if err != nil {
					t.Fatalf("构造过期二维码失败: %v", err)
				}
				return s
			},
			want: protocol.ErrSGIDExpired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeTitleSession{}
			aime, calls := fakeAimeServer(t)
			svc := newTestService(t, session, aime.URL)

			_, err := svc.Fetch(context.Background(), tc.sgid(t))
			if !errors.Is(err, tc.want) {
				t.Errorf("错误 = %v, 期望 %v", err, tc.want)
			}
			if calls.Load() != 0 {
				t.Errorf("不应调用 AimeDB，实际 %d 次", calls.Load())
			}
			if session.loginCalls.Load() != 0 {
				t.Error("不应登录机台")
			}
		})
	}
}

// TestFetchIsSerialized 断言并发调用被串行化。
//
// 同账号并发登录会互相挤掉，机台侧也会直接拒绝，因此必须互斥。
func TestFetchIsSerialized(t *testing.T) {
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64

	session := &fakeTitleSession{scores: []model.Score{{MusicID: 1, PlayCount: 1}}}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	// 用一个会观察并发度的机台客户端替换默认实现。
	svc.newTitle = func(protocol.TitleOptions) (titleSession, error) {
		return &concurrencyProbe{session: session, concurrent: &concurrent, max: &maxConcurrent}, nil
	}

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = svc.Fetch(context.Background(), validSGID(t))
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	if got := maxConcurrent.Load(); got > 1 {
		t.Errorf("最大并发会话数 = %d, 期望 1（同账号操作必须串行）", got)
	}
}

// concurrencyProbe 在 Login 与 Logout 之间记录并发会话数。
type concurrencyProbe struct {
	session    *fakeTitleSession
	concurrent *atomic.Int64
	max        *atomic.Int64
}

func (c *concurrencyProbe) Login(context.Context, protocol.Credential, int) error {
	n := c.concurrent.Add(1)
	for {
		old := c.max.Load()
		if n <= old || c.max.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	return nil
}

func (c *concurrencyProbe) Logout(context.Context) error {
	time.Sleep(time.Millisecond)
	c.concurrent.Add(-1)
	return nil
}

func (c *concurrencyProbe) Music(context.Context) ([]model.Score, error) {
	return c.session.Music(context.Background())
}

func (c *concurrencyProbe) Profile(context.Context) (protocol.Profile, error) {
	return protocol.Profile{}, nil
}

// TestProfileUsesSameSessionLifecycle 断言 profile 也走「登录 → 拉取 → 登出」。
func TestProfileUsesSameSessionLifecycle(t *testing.T) {
	session := &fakeTitleSession{profile: protocol.Profile{UserID: 1, UserName: "玩家", Rating: 15327}}
	aime, _ := fakeAimeServer(t)
	svc := newTestService(t, session, aime.URL)

	profile, err := svc.Profile(context.Background(), validSGID(t))
	if err != nil {
		t.Fatalf("Profile 报错: %v", err)
	}
	if profile.Rating != 15327 {
		t.Errorf("Rating = %d, 期望 15327", profile.Rating)
	}
	if session.loginCalls.Load() != 1 || session.logoutCalls.Load() != 1 {
		t.Errorf("登录/登出次数 = %d/%d, 期望 1/1", session.loginCalls.Load(), session.logoutCalls.Load())
	}
}

// TestNewRejectsUnknownVersion 断言未知版本在构造时就报参数错。
func TestNewRejectsUnknownVersion(t *testing.T) {
	_, err := New(Options{Version: "9.99"})
	if err == nil {
		t.Fatal("未知版本应当报错")
	}
	if !model.IsKind(err, model.KindParam) {
		t.Errorf("应归为参数错误（退出码 3），得到 %v", err)
	}
}

// TestNewRejectsBadProxy 断言非法代理在构造时暴露出来。
func TestNewRejectsBadProxy(t *testing.T) {
	if _, err := New(Options{ProxyURL: "://bad"}); err == nil {
		t.Error("非法代理地址应当报错")
	}
}

// ---------------------------------------------------------------------------
// 版本过期与 IP 阻断的区分
// ---------------------------------------------------------------------------

// TestProbeClassifiesStaleVersion 断言「配置版本过期」与「出口 IP 被阻断」被分开判定。
//
// 两者在传输层完全同形（HTTP 200 + 0 字节），但处置方式相反：
// 前者换 --version 即可，后者换网络才有用。分不开就会把人引向错误的排查方向。
func TestProbeClassifiesStaleVersion(t *testing.T) {
	attempts := []versionAttempt{
		{version: "1.53", err: protocol.ErrEmptyResponse},
		{version: "1.55", err: nil},
	}

	got := titleVerdictFromAttempts("1.53", attempts)

	if got.Class != ClassVersionStale {
		t.Fatalf("Class = %q, 期望 %q", got.Class, ClassVersionStale)
	}
	if got.OK {
		t.Error("配置版本不可用时不应判为通过")
	}
	if got.AcceptedVersion != "1.55" {
		t.Errorf("可用版本 = %q, 期望 1.55", got.AcceptedVersion)
	}
	if !strings.Contains(got.Hint, "1.55") {
		t.Errorf("提示应指明改用哪个版本: %s", got.Hint)
	}
	if !strings.Contains(got.Hint, "不是出口 IP 问题") {
		t.Errorf("提示应明确排除 IP 问题，否则使用者仍会去换网络: %s", got.Hint)
	}
	if got.Class.exitCode() != 3 {
		t.Errorf("退出码 = %d, 期望 3（参数错）", got.Class.exitCode())
	}
	// 明细里要能直接看出各版本分别是什么结果。
	if !strings.Contains(got.Detail, "1.53=0 字节") || !strings.Contains(got.Detail, "1.55=正常") {
		t.Errorf("明细应逐版本给出结果: %s", got.Detail)
	}
}

// TestProbeClassifiesAllEmptyAsBlocked 断言所有版本都拿不到响应体时才判为阻断。
func TestProbeClassifiesAllEmptyAsBlocked(t *testing.T) {
	attempts := []versionAttempt{
		{version: "1.55", err: protocol.ErrEmptyResponse},
		{version: "1.53", err: protocol.ErrEmptyResponse},
	}

	got := titleVerdictFromAttempts("1.55", attempts)

	if got.Class != ClassBlocked {
		t.Fatalf("Class = %q, 期望 %q", got.Class, ClassBlocked)
	}
	if got.Class.exitCode() != 2 {
		t.Errorf("退出码 = %d, 期望 2", got.Class.exitCode())
	}
	if !strings.Contains(got.Hint, "IP") {
		t.Errorf("提示应指向 IP 问题: %s", got.Hint)
	}
	if got.AcceptedVersion != "" {
		t.Errorf("没有可用版本时不应报告可用版本: %q", got.AcceptedVersion)
	}
}

// TestProbeClassifiesConfiguredVersionWorking 断言配置版本可用时判为通过。
func TestProbeClassifiesConfiguredVersionWorking(t *testing.T) {
	attempts := []versionAttempt{{version: "1.55", err: nil}}

	got := titleVerdictFromAttempts("1.55", attempts)

	if got.Class != ClassOK || !got.OK {
		t.Errorf("Class = %q OK = %v, 期望 ok/true", got.Class, got.OK)
	}
	if got.Class.exitCode() != 0 {
		t.Errorf("退出码 = %d, 期望 0", got.Class.exitCode())
	}
}

// TestProbeClassifiesDecryptFailure 断言全部版本都解不开时判为参数问题。
func TestProbeClassifiesDecryptFailure(t *testing.T) {
	attempts := []versionAttempt{
		{version: "1.55", err: protocol.ErrDecrypt},
		{version: "1.53", err: protocol.ErrDecrypt},
	}

	got := titleVerdictFromAttempts("1.55", attempts)
	if got.Class != ClassParams {
		t.Errorf("Class = %q, 期望 %q", got.Class, ClassParams)
	}
}

// TestProbeClassifiesNetworkFailure 断言网络类错误原样上报，不被误判成版本问题。
func TestProbeClassifiesNetworkFailure(t *testing.T) {
	attempts := []versionAttempt{{version: "1.55", err: protocol.ErrNetwork}}

	got := titleVerdictFromAttempts("1.55", attempts)
	if got.Class != ClassNetwork {
		t.Errorf("Class = %q, 期望 %q", got.Class, ClassNetwork)
	}
}

// TestProbeClassifiesEmptyAttemptList 断言没有任何候选版本时给出明确错误。
func TestProbeClassifiesEmptyAttemptList(t *testing.T) {
	got := titleVerdictFromAttempts("1.55", nil)
	if got.Class != ClassParams {
		t.Errorf("Class = %q, 期望 %q", got.Class, ClassParams)
	}
	if !strings.Contains(got.Detail, "协议版本") {
		t.Errorf("应说明是版本参数表的问题: %s", got.Detail)
	}
}

// TestIsVersionDiagnosable 断言只有「换版本可能有用」的错误才值得继续试。
func TestIsVersionDiagnosable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"0 字节可能只是版本不被接受", protocol.ErrEmptyResponse, true},
		{"解密失败可能换版本可解", protocol.ErrDecrypt, true},
		{"能解密但不是 JSON 同理", protocol.ErrVersionMismatch, true},
		{"网络不通换版本没意义", protocol.ErrNetwork, false},
		{"超时同理", protocol.ErrTimeout, false},
		{"业务错同理", protocol.ErrAlreadyLoggedIn, false},
		{"nil", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVersionDiagnosable(tc.err); got != tc.want {
				t.Errorf("isVersionDiagnosable(%v) = %v, 期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestProbeCandidatesIncludeConfiguredAndNewest 断言候选里既有配置版本也有最新版本。
//
// 少了配置版本就无法判断「是配置错了还是服务变了」；少了最新版本就发现不了版本更新。
func TestProbeCandidatesIncludeConfiguredAndNewest(t *testing.T) {
	svc, err := New(Options{Version: "1.53", Timeout: time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	candidates := svc.probeCandidates()
	if candidates[0] != "1.53" {
		t.Errorf("首个候选 = %q, 期望配置的 1.53", candidates[0])
	}
	found := false
	for _, name := range candidates {
		if name == "1.55" {
			found = true
		}
	}
	if !found {
		t.Errorf("候选里应包含最新版本 1.55: %v", candidates)
	}
}

// TestCombineTreatsStaleVersionAsFailure 断言版本过期不会被当成正常。
//
// 严重度表漏项的后果是取到零值、视同正常，把故障静默降级为通过。
func TestCombineTreatsStaleVersionAsFailure(t *testing.T) {
	for _, order := range [][2]FaultClass{
		{ClassVersionStale, ClassOK},
		{ClassOK, ClassVersionStale},
	} {
		if got := combine(order[0], order[1]); got != ClassVersionStale {
			t.Errorf("combine(%q, %q) = %q, 期望 %q", order[0], order[1], got, ClassVersionStale)
		}
	}

	// 每个类别都必须在严重度表里有明确取值，不能靠零值兜底。
	for _, class := range []FaultClass{
		ClassOK, ClassBusiness, ClassNetwork, ClassBlocked, ClassParams, ClassVersionStale,
	} {
		if got := combine(ClassOK, class); class != ClassOK && got != class {
			t.Errorf("combine(ok, %q) = %q, 期望 %q（严重度表可能漏了这一类）", class, got, class)
		}
	}
}
