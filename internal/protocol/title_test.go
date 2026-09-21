package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// newTestTransport 构造测试用传输客户端。
func newTestTransport(t *testing.T) *transport.Client {
	t.Helper()
	hc, err := transport.New(transport.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造传输客户端失败: %v", err)
	}
	return hc
}

// recordedRequest 是一次被记录的机台请求。
type recordedRequest struct {
	apiName string
	path    string
	headers http.Header
	body    []byte
}

// fakeTitleServer 模拟标题服务器：解密请求、按需应答、记录一切。
//
// 单测禁止访问真实接口，所有协议行为都在这里被断言。
type fakeTitleServer struct {
	t       *testing.T
	version Version

	mu       sync.Mutex
	requests []recordedRequest

	// respond 按 API 名与解密后的请求体给出响应；返回的 body 会原样写出（需自行打包）。
	respond func(apiName string, reqBody []byte) (status int, body []byte)

	baseURL string
}

// newFakeTitleServer 启动假机台；respond 为 nil 时统一回 {"returnCode":1}。
func newFakeTitleServer(t *testing.T, version Version, respond func(string, []byte) (int, []byte)) *fakeTitleServer {
	t.Helper()
	f := &fakeTitleServer{t: t, version: version, respond: respond}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.baseURL = server.URL + "/Maimai2Servlet/"
	return f
}

// ServeHTTP 实现机台的服务端行为。
func (f *fakeTitleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("读取请求体失败: %v", err)
	}

	plain, decryptErr := unpackBody(raw, f.version)
	apiName := ""
	if decryptErr == nil {
		apiName = f.apiNameFor(r.URL.Path)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{apiName: apiName, path: r.URL.Path, headers: r.Header, body: plain})
	f.mu.Unlock()

	if f.respond == nil {
		f.writePacked(w, http.StatusOK, map[string]any{"returnCode": 1})
		return
	}
	status, body := f.respond(apiName, plain)
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		f.t.Errorf("写出响应失败: %v", err)
	}
}

// writePacked 把响应加密后写出。
func (f *fakeTitleServer) writePacked(w http.ResponseWriter, status int, payload any) {
	f.t.Helper()
	body, err := packBody(payload, f.version)
	if err != nil {
		f.t.Errorf("打包响应失败: %v", err)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		f.t.Errorf("写出响应失败: %v", err)
	}
}

// apiNameFor 用参数表反查路径里的 api_hash 对应哪个 API。
func (f *fakeTitleServer) apiNameFor(path string) string {
	hash := path[len("/Maimai2Servlet/"):]
	for _, name := range []string{APIPing, APIUserLogin, APIUserLogout, APIGetUserMusic, APIGetUserData, APIGetUserPreview} {
		if apiHash(name, f.version.ObfuscateParam) == hash {
			return name
		}
	}
	return ""
}

// requestsFor 返回某个 API 收到的请求。
func (f *fakeTitleServer) requestsFor(apiName string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.apiName == apiName {
			out = append(out, r)
		}
	}
	return out
}

// count 返回收到的请求总数。
func (f *fakeTitleServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// newTestClient 构造指向假机台的客户端。
func (f *fakeTitleServer) newTestClient(t *testing.T) *TitleClient {
	t.Helper()
	client, err := NewTitleClient(TitleOptions{
		HTTP:    newTestTransport(t),
		Version: f.version,
		BaseURL: f.baseURL,
	})
	if err != nil {
		t.Fatalf("构造 TitleClient 失败: %v", err)
	}
	return client
}

// TestPingRequestShape 断言机台请求的路径、hasher 头与请求头集合完全符合协议要求。
func TestPingRequestShape(t *testing.T) {
	for _, name := range []string{"1.53", "1.55"} {
		t.Run(name, func(t *testing.T) {
			version := Versions[name]
			server := newFakeTitleServer(t, version, nil)
			client := server.newTestClient(t)

			if err := client.Ping(context.Background()); err != nil {
				t.Fatalf("Ping 报错: %v", err)
			}

			requests := server.requestsFor(APIPing)
			if len(requests) != 1 {
				t.Fatalf("Ping 请求数 = %d, 期望 1", len(requests))
			}
			req := requests[0]

			wantHash := apiHash(APIPing, version.ObfuscateParam)
			if want := "/Maimai2Servlet/" + wantHash; req.path != want {
				t.Errorf("路径 = %q, 期望 %q", req.path, want)
			}
			// agent_id 是 userId；未登录时为 0。
			if want := wantHash + "#0"; req.headers.Get("User-Agent") != want {
				t.Errorf("User-Agent = %q, 期望 %q", req.headers.Get("User-Agent"), want)
			}
			if got := req.headers.Get("Mai-Encoding"); got != version.Encoding {
				t.Errorf("Mai-Encoding = %q, 期望 %q", got, version.Encoding)
			}
			if got := req.headers.Get("Content-Encoding"); got != "deflate" {
				t.Errorf("Content-Encoding = %q, 期望 deflate", got)
			}
			if got := req.headers.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, 期望 application/json", got)
			}
			if got := req.headers.Get("Charset"); got != "UTF-8" {
				t.Errorf("Charset = %q, 期望 UTF-8", got)
			}
			if got := req.headers.Get("Expect"); got != "100-continue" {
				t.Errorf("Expect = %q, 期望 100-continue", got)
			}
			// Accept-Encoding 必须显式存在且为空，用于禁用压缩协商。
			values, present := req.headers["Accept-Encoding"]
			if !present {
				t.Error("Accept-Encoding 头必须显式存在")
			} else if len(values) != 1 || values[0] != "" {
				t.Errorf("Accept-Encoding = %v, 期望单个空串", values)
			}
		})
	}
}

// TestCallClassifiesEmptyResponse 断言 0 字节响应被识别为「出口 IP 被阻断」。
//
// 这是 §5.6 四类故障里的第二类，也是本项目最常遇到的部署问题。
func TestCallClassifiesEmptyResponse(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(string, []byte) (int, []byte) {
		return http.StatusOK, nil
	})
	client := server.newTestClient(t)

	err := client.Ping(context.Background())
	if err == nil {
		t.Fatal("空响应应当报错")
	}
	if !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("应识别为空响应，得到 %v", err)
	}
	if !IsKind(err, KindNetwork) {
		t.Error("空响应应归入网络分类（退出码 2）")
	}
}

// TestCallClassifiesDecryptFailure 断言解不开的响应被识别为版本不匹配。
func TestCallClassifiesDecryptFailure(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		// 结尾填充字节非法，必然无法通过 PKCS7 校验。
		{"填充非法导致解密失败", append(bytes.Repeat([]byte{0xAB}, 15), 0x00)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeTitleServer(t, Versions["1.53"], func(string, []byte) (int, []byte) {
				return http.StatusOK, tc.body
			})
			client := server.newTestClient(t)

			err := client.Ping(context.Background())
			if !errors.Is(err, ErrDecrypt) {
				t.Errorf("应识别为解密失败，得到 %v", err)
			}
			if !IsKind(err, KindParam) {
				t.Error("解密失败应归入参数分类（退出码 3）")
			}
		})
	}
}

// TestCallClassifiesGarbledPayload 断言能解密但不是 JSON 时判为版本不匹配。
func TestCallClassifiesGarbledPayload(t *testing.T) {
	version := Versions["1.53"]
	server := newFakeTitleServer(t, version, func(string, []byte) (int, []byte) {
		// 用正确参数加密一段非 JSON 数据：解密会成功，反序列化必然失败。
		packed, err := packBytes([]byte("<html>网关拦截页</html>"), version)
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, packed
	})
	client := server.newTestClient(t)

	err := client.Ping(context.Background())
	if !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("应识别为版本不匹配，得到 %v", err)
	}
}

// TestLoginReturnCodes 断言登录返回码被翻译成对应错误。
func TestLoginReturnCodes(t *testing.T) {
	tests := []struct {
		name       string
		returnCode int
		wantErr    error
		wantClass  Kind
	}{
		{"成功", 1, nil, 0},
		{"已登录", 100, ErrAlreadyLoggedIn, KindBusiness},
		{"二维码过期", 102, nil, KindBusiness},
		{"KeyChip 不匹配", 110, nil, KindBusiness},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeTitleServer(t, Versions["1.53"], func(string, []byte) (int, []byte) {
				packed, err := packBody(map[string]any{"returnCode": tc.returnCode}, Versions["1.53"])
				if err != nil {
					t.Fatalf("打包失败: %v", err)
				}
				return http.StatusOK, packed
			})
			client := server.newTestClient(t)

			err := client.Login(context.Background(), Credential{UserID: 10807675, Token: "token-abc"}, 0)
			if tc.wantErr == nil && tc.returnCode == 1 {
				if err != nil {
					t.Fatalf("成功登录不应报错: %v", err)
				}
				if !client.HasSession() {
					t.Error("登录成功后应标记为已建立会话")
				}
				return
			}
			if err == nil {
				t.Fatal("非成功返回码应当报错")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("错误 = %v, 期望匹配 %v", err, tc.wantErr)
			}
			if !IsKind(err, tc.wantClass) {
				t.Errorf("错误分类不符: %v", err)
			}
			if client.HasSession() {
				t.Error("登录未成功时不应标记会话")
			}
		})
	}
}

// TestLoginFailureDoesNotLogout 断言登录失败后不会发出登出请求。
//
// 此时并没有属于本次的会话，用错误的 loginDateTime 登出既清不掉别人的会话，
// 又可能把自己推到更糟的状态。
func TestLoginFailureDoesNotLogout(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(apiName string, _ []byte) (int, []byte) {
		code := 1
		if apiName == APIUserLogin {
			code = 100
		}
		packed, err := packBody(map[string]any{"returnCode": code}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, packed
	})
	client := server.newTestClient(t)

	if err := client.Login(context.Background(), Credential{UserID: 1, Token: "t"}, 0); err == nil {
		t.Fatal("应返回已登录错误")
	}
	if err := client.Logout(context.Background()); err != nil {
		t.Fatalf("未建立会话时 Logout 应直接返回: %v", err)
	}
	if got := len(server.requestsFor(APIUserLogout)); got != 0 {
		t.Errorf("登出请求数 = %d, 期望 0", got)
	}
}

// TestLogoutReusesLoginDateTime 断言登出携带与登录完全相同的 dateTime。
//
// loginDateTime 不一致会让服务端拒绝登出，会话残留到 15 分钟硬超时。
func TestLogoutReusesLoginDateTime(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], nil)
	client := server.newTestClient(t)

	cred := Credential{UserID: 10807675, Token: "token-abc"}
	if err := client.Login(context.Background(), cred, 0); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if err := client.Logout(context.Background()); err != nil {
		t.Fatalf("登出失败: %v", err)
	}

	logins := server.requestsFor(APIUserLogin)
	logouts := server.requestsFor(APIUserLogout)
	if len(logins) != 1 || len(logouts) != 1 {
		t.Fatalf("登录/登出请求数 = %d/%d, 期望 1/1", len(logins), len(logouts))
	}

	var loginBody struct {
		UserID   int   `json:"userId"`
		DateTime int64 `json:"dateTime"`
	}
	if err := json.Unmarshal(logins[0].body, &loginBody); err != nil {
		t.Fatalf("解析登录请求失败: %v", err)
	}
	var logoutBody struct {
		UserID        int   `json:"userId"`
		LoginDateTime int64 `json:"loginDateTime"`
	}
	if err := json.Unmarshal(logouts[0].body, &logoutBody); err != nil {
		t.Fatalf("解析登出请求失败: %v", err)
	}

	if loginBody.DateTime != logoutBody.LoginDateTime {
		t.Errorf("loginDateTime = %d, 期望与登录的 dateTime %d 一致", logoutBody.LoginDateTime, loginBody.DateTime)
	}
	if loginBody.UserID != cred.UserID || logoutBody.UserID != cred.UserID {
		t.Errorf("userId 不一致: 登录 %d, 登出 %d", loginBody.UserID, logoutBody.UserID)
	}
	// 登出后的 User-Agent 应带上 userId。
	if got := logouts[0].headers.Get("User-Agent"); !bytes.HasSuffix([]byte(got), []byte("#10807675")) {
		t.Errorf("登出 User-Agent = %q, 期望以 #10807675 结尾", got)
	}
}

// TestLoginRequestFieldNamesPerVersion 断言登录请求体的字段名与时间单位符合该版本的形状。
//
// 这里必须按版本断言：`acsessCode` 与 `accessCode` 都真实存在过，用的是哪一个由版本参数决定。
// 写错字段名或时间单位，服务端不会回业务错误码，而是直接抛 HTTP 500。
func TestLoginRequestFieldNamesPerVersion(t *testing.T) {
	tests := []struct {
		version       string
		accessField   string
		wrongField    string
		timestampUnit string
		wantContinue  bool
		wantRegionID  float64
		wantLoginDT   bool
	}{
		{
			version: "1.53", accessField: "acsessCode", wrongField: "accessCode",
			timestampUnit: "毫秒", wantContinue: true, wantRegionID: 0, wantLoginDT: false,
		},
		{
			version: "1.55", accessField: "accessCode", wrongField: "acsessCode",
			timestampUnit: "秒", wantContinue: false, wantRegionID: 8, wantLoginDT: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			version := Versions[tc.version]
			server := newFakeTitleServer(t, version, nil)
			client := server.newTestClient(t)

			before := time.Now()
			if err := client.Login(context.Background(), Credential{UserID: 42, Token: "tok"}, 0); err != nil {
				t.Fatalf("登录失败: %v", err)
			}
			after := time.Now()

			var raw map[string]any
			if err := json.Unmarshal(server.requestsFor(APIUserLogin)[0].body, &raw); err != nil {
				t.Fatalf("解析请求体失败: %v", err)
			}

			// 基础字段：两种形状都必须有。
			for _, key := range []string{"userId", "regionId", "dateTime", "placeId", "clientId", "token", "isContinue", "genericFlag"} {
				if _, ok := raw[key]; !ok {
					t.Errorf("登录请求缺少字段 %q", key)
				}
			}

			// 访问码字段名随版本变化。
			if _, ok := raw[tc.accessField]; !ok {
				t.Errorf("登录请求缺少访问码字段 %q", tc.accessField)
			}
			if _, ok := raw[tc.wrongField]; ok {
				t.Errorf("版本 %s 不应出现字段 %q", tc.version, tc.wrongField)
			}

			// 时间戳单位：秒会落在秒级区间，毫秒在毫秒级区间。
			dateTime, ok := raw["dateTime"].(float64)
			if !ok {
				t.Fatalf("dateTime 不是数字: %v", raw["dateTime"])
			}
			seconds := int64(dateTime)
			if tc.timestampUnit == "秒" {
				if seconds < before.Unix()-5 || seconds > after.Unix()+5 {
					t.Errorf("dateTime = %d, 期望秒级时间戳（%d 附近）", seconds, before.Unix())
				}
			} else {
				if seconds < before.UnixMilli()-5000 || seconds > after.UnixMilli()+5000 {
					t.Errorf("dateTime = %d, 期望毫秒级时间戳（%d 附近）", seconds, before.UnixMilli())
				}
			}

			if got := raw["isContinue"]; got != tc.wantContinue {
				t.Errorf("isContinue = %v, 期望 %v", got, tc.wantContinue)
			}
			if got := raw["regionId"]; got != tc.wantRegionID {
				t.Errorf("regionId = %v, 期望 %v", got, tc.wantRegionID)
			}
			if _, ok := raw["loginDateTime"]; ok != tc.wantLoginDT {
				t.Errorf("loginDateTime 是否存在 = %v, 期望 %v", ok, tc.wantLoginDT)
			}
		})
	}
}

// TestLogoutRequestFieldNamesPerVersion 断言登出请求体的字段名符合该版本的形状。
//
// loginDateTime 必须与登录时的 dateTime 完全相同，否则服务端拒绝登出、会话残留到硬超时。
func TestLogoutRequestFieldNamesPerVersion(t *testing.T) {
	for _, name := range []string{"1.53", "1.55"} {
		t.Run(name, func(t *testing.T) {
			version := Versions[name]
			server := newFakeTitleServer(t, version, nil)
			client := server.newTestClient(t)

			cred := Credential{UserID: 10807675, Token: "token-abc"}
			if err := client.Login(context.Background(), cred, 0); err != nil {
				t.Fatalf("登录失败: %v", err)
			}
			if err := client.Logout(context.Background()); err != nil {
				t.Fatalf("登出失败: %v", err)
			}

			var loginBody struct {
				DateTime int64 `json:"dateTime"`
			}
			if err := json.Unmarshal(server.requestsFor(APIUserLogin)[0].body, &loginBody); err != nil {
				t.Fatalf("解析登录请求失败: %v", err)
			}
			var logoutRaw map[string]any
			if err := json.Unmarshal(server.requestsFor(APIUserLogout)[0].body, &logoutRaw); err != nil {
				t.Fatalf("解析登出请求失败: %v", err)
			}

			logoutDT, ok := logoutRaw["loginDateTime"].(float64)
			if !ok {
				t.Fatalf("登出请求缺少 loginDateTime: %v", logoutRaw)
			}
			if int64(logoutDT) != loginBody.DateTime {
				t.Errorf("loginDateTime = %d, 期望与登录的 dateTime %d 一致", int64(logoutDT), loginBody.DateTime)
			}

			// 新版本的登出还要带 regionId / accessCode / type。
			if version.Login.LogoutType != 0 {
				for _, key := range []string{"regionId", version.Login.AccessCodeField, "type", "placeId", "clientId"} {
					if _, ok := logoutRaw[key]; !ok {
						t.Errorf("版本 %s 的登出请求缺少字段 %q", name, key)
					}
				}
			}
		})
	}
}

// TestRequestHeadersIncludeNumber 断言请求带上 number 头（参考实现会带，服务端可能据此区分来源）。
func TestRequestHeadersIncludeNumber(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.55"], nil)
	client := server.newTestClient(t)

	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 报错: %v", err)
	}
	if got := server.requestsFor(APIPing)[0].headers.Get("number"); got != "0" {
		t.Errorf("number 头 = %q, 期望 \"0\"", got)
	}
}

// TestEveryVersionHasLoginShape 断言参数表里每个版本都带完整的登录形状。
func TestEveryVersionHasLoginShape(t *testing.T) {
	for _, name := range SupportedVersions() {
		t.Run(name, func(t *testing.T) {
			version := Versions[name]
			if version.Login.AccessCodeField == "" {
				t.Error("缺少访问码字段名")
			}
			if version.Login.AccessCodeField != "accessCode" && version.Login.AccessCodeField != "acsessCode" {
				t.Errorf("访问码字段名 %q 不是已知的两种拼写之一", version.Login.AccessCodeField)
			}
			if err := version.Validate(); err != nil {
				t.Errorf("Validate 报错: %v", err)
			}
		})
	}
}

// TestMusicPaginationAndFiltering 断言分页累积与 playCount<=0 过滤。
func TestMusicPaginationAndFiltering(t *testing.T) {
	pages := map[int]any{
		0: map[string]any{
			"nextIndex": 7,
			"userMusicList": []any{
				map[string]any{"userMusicDetailList": []any{
					map[string]any{"musicId": 1001, "level": 3, "playCount": 5, "achievement": 1010000, "comboStatus": 3, "syncStatus": 5, "deluxscoreMax": 2711},
					// playCount 为 0 表示未游玩，必须丢弃。
					map[string]any{"musicId": 1002, "level": 2, "playCount": 0, "achievement": 0},
				}},
			},
		},
		7: map[string]any{
			"nextIndex": 0,
			"userMusicList": []any{
				map[string]any{"userMusicDetailList": []any{
					map[string]any{"musicId": 1003, "level": 4, "playCount": 1, "achievement": 995000, "comboStatus": 1, "syncStatus": 2},
				}},
			},
		},
	}

	server := newFakeTitleServer(t, Versions["1.53"], func(apiName string, reqBody []byte) (int, []byte) {
		packed, err := packBody(map[string]any{"returnCode": 1}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		if apiName != APIGetUserMusic {
			return http.StatusOK, packed
		}
		var req struct {
			NextIndex int `json:"nextIndex"`
			MaxCount  int `json:"maxCount"`
		}
		if err := json.Unmarshal(reqBody, &req); err != nil {
			t.Fatalf("解析分页请求失败: %v", err)
		}
		page, ok := pages[req.NextIndex]
		if !ok {
			t.Fatalf("意外的 nextIndex=%d", req.NextIndex)
		}
		body, err := packBody(page, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, body
	})
	client := server.newTestClient(t)

	if err := client.Login(context.Background(), Credential{UserID: 7, Token: "t"}, 0); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	scores, err := client.Music(context.Background())
	if err != nil {
		t.Fatalf("拉取成绩失败: %v", err)
	}

	if len(scores) != 2 {
		t.Fatalf("成绩条数 = %d, 期望 2（未游玩条目已被过滤）", len(scores))
	}
	if scores[0].MusicID != 1001 || scores[0].Achievement != 101.0 {
		t.Errorf("首条成绩 = %+v, 期望 musicId=1001 achievement=101", scores[0])
	}
	if scores[0].Combo != model.ComboAP || scores[0].Sync != model.SyncFullSync || scores[0].DXScore != 2711 {
		t.Errorf("首条成绩的竞速标识或 DX 分不符: %+v", scores[0])
	}
	if scores[1].MusicID != 1003 || scores[1].Achievement != 99.5 {
		t.Errorf("第二条成绩 = %+v, 期望 musicId=1003 achievement=99.5", scores[1])
	}

	if got := server.requestsFor(APIGetUserMusic); len(got) != 2 {
		t.Errorf("分页请求数 = %d, 期望 2", len(got))
	}
}

// TestMusicRejectsStuckCursor 断言 nextIndex 不推进时报错而不是无限循环。
func TestMusicRejectsStuckCursor(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(apiName string, _ []byte) (int, []byte) {
		if apiName != APIGetUserMusic {
			packed, err := packBody(map[string]any{"returnCode": 1}, Versions["1.53"])
			if err != nil {
				t.Fatalf("打包失败: %v", err)
			}
			return http.StatusOK, packed
		}
		// 永远返回同一个非零 nextIndex。
		body, err := packBody(map[string]any{
			"nextIndex":     1,
			"userMusicList": []any{map[string]any{"userMusicDetailList": []any{}}},
		}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, body
	})
	client := server.newTestClient(t)

	if err := client.Login(context.Background(), Credential{UserID: 7, Token: "t"}, 0); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if _, err := client.Music(context.Background()); err == nil {
		t.Fatal("游标不推进时应当报错")
	}
	if got := len(server.requestsFor(APIGetUserMusic)); got > 3 {
		t.Errorf("分页请求数 = %d, 说明没有及时止损", got)
	}
}

// TestMusicRequiresLogin 断言未登录时拉成绩会被拒绝。
func TestMusicRequiresLogin(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], nil)
	client := server.newTestClient(t)

	if _, err := client.Music(context.Background()); err == nil {
		t.Fatal("未登录时拉成绩应当报错")
	}
	if _, err := client.Profile(context.Background()); err == nil {
		t.Fatal("未登录时拉资料应当报错")
	}
	if server.count() != 0 {
		t.Errorf("不应发出任何请求，实际 %d 次", server.count())
	}
}

// TestProfileToleratesPartialFailure 断言单个资料接口失败不会让整条命令失败。
func TestProfileToleratesPartialFailure(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(apiName string, _ []byte) (int, []byte) {
		switch apiName {
		case APIGetUserPreview:
			// 模拟该接口在部分版本不可用。
			packed, err := packBody(map[string]any{"returnCode": 3}, Versions["1.53"])
			if err != nil {
				t.Fatalf("打包失败: %v", err)
			}
			return http.StatusOK, packed
		case APIGetUserData:
			packed, err := packBody(map[string]any{
				"returnCode":    1,
				"userId":        10807675,
				"userName":      "测试玩家",
				"rating":        15327,
				"highestRating": 15327,
				"userRatingList": []any{
					map[string]any{"musicId": 1001, "level": 3, "point": 321},
				},
			}, Versions["1.53"])
			if err != nil {
				t.Fatalf("打包失败: %v", err)
			}
			return http.StatusOK, packed
		}
		packed, err := packBody(map[string]any{"returnCode": 1}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, packed
	})
	client := server.newTestClient(t)

	if err := client.Login(context.Background(), Credential{UserID: 10807675, Token: "t"}, 0); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	profile, err := client.Profile(context.Background())
	if err != nil {
		t.Fatalf("单个接口失败不应让 Profile 报错: %v", err)
	}
	if profile.Rating != 15327 || profile.UserName != "测试玩家" {
		t.Errorf("应保留可用接口的数据: %+v", profile)
	}
	if len(profile.RatingList) != 1 || profile.RatingList[0].Point != 321 {
		t.Errorf("ratingList 解析不符: %+v", profile.RatingList)
	}
}

// TestPingBusinessErrorIsNotVersionMismatch 断言业务码不会被误判成版本问题。
func TestPingBusinessErrorIsNotVersionMismatch(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(string, []byte) (int, []byte) {
		packed, err := packBody(map[string]any{"returnCode": 100}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, packed
	})
	client := server.newTestClient(t)

	err := client.Ping(context.Background())
	if err == nil {
		t.Fatal("非成功返回码应当报错")
	}
	if errors.Is(err, ErrVersionMismatch) {
		t.Error("业务码不应被归为版本不匹配")
	}
	if !IsKind(err, KindBusiness) {
		t.Errorf("应归为业务分类，得到 %v", err)
	}
}

// TestPingToleratesUnknownResponseShape 断言响应里没有 returnCode 时按成功处理。
//
// 机台响应结构随版本漂移，字段变动不该被当成故障。
func TestPingToleratesUnknownResponseShape(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(string, []byte) (int, []byte) {
		packed, err := packBody(map[string]any{"somethingElse": "ok"}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, packed
	})
	client := server.newTestClient(t)

	if err := client.Ping(context.Background()); err != nil {
		t.Errorf("缺少 returnCode 时不应报错: %v", err)
	}
}

// TestMusicRequestUsesConfiguredPageSize 断言分页请求带上文档约定的大 maxCount。
func TestMusicRequestUsesConfiguredPageSize(t *testing.T) {
	server := newFakeTitleServer(t, Versions["1.53"], func(apiName string, _ []byte) (int, []byte) {
		if apiName == APIGetUserMusic {
			body, err := packBody(map[string]any{"nextIndex": 0, "userMusicList": []any{}}, Versions["1.53"])
			if err != nil {
				t.Fatalf("打包失败: %v", err)
			}
			return http.StatusOK, body
		}
		packed, err := packBody(map[string]any{"returnCode": 1}, Versions["1.53"])
		if err != nil {
			t.Fatalf("打包失败: %v", err)
		}
		return http.StatusOK, packed
	})
	client := server.newTestClient(t)

	if err := client.Login(context.Background(), Credential{UserID: 7, Token: "t"}, 0); err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if _, err := client.Music(context.Background()); err != nil {
		t.Fatalf("拉取成绩失败: %v", err)
	}

	requests := server.requestsFor(APIGetUserMusic)
	if len(requests) != 1 {
		t.Fatalf("分页请求数 = %d, 期望 1", len(requests))
	}
	var req map[string]any
	if err := json.Unmarshal(requests[0].body, &req); err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	if got := req["maxCount"]; got != float64(musicPageSize) {
		t.Errorf("maxCount = %v, 期望 %d", got, musicPageSize)
	}
	if got := req["userId"]; got != float64(7) {
		t.Errorf("userId = %v, 期望 7", got)
	}
}
