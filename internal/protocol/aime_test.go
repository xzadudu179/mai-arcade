package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAimeSignature 用独立算出的期望值断言 key 的构造方式。
//
// 期望值由 Python hashlib 单独算出，不是本实现的产物，因此能真正抓出算法偏差。
func TestAimeSignature(t *testing.T) {
	tests := []struct {
		name      string
		chipID    string
		timestamp string
		want      string
	}{
		{
			name:      "1.53 机台标识",
			chipID:    "A63E-01C28055905",
			timestamp: "250101120000",
			want:      "CEAC14F4B933A3821F26B0A62C3FAAC7E1C4FC8B13F39A68674EF9A2D892B175",
		},
		{
			name:      "旧版机台标识",
			chipID:    "A63E-01E68606624",
			timestamp: "250101120000",
			want:      "C13A90616D18EDE7E0F2836EED8626417B27D938432A84C100B60C84B94F1C05",
		},
		{
			name:      "探针使用的固定时刻",
			chipID:    "A63E-01C28055905",
			timestamp: "260920205100",
			want:      "9D26809AE8ED122A401C0A96E9E12AC8C00108377912B39296CF1F2AD0C7C1ED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := aimeSignature(tc.chipID, tc.timestamp, AimeCommonKey)
			if got != tc.want {
				t.Errorf("aimeSignature = %s, 期望 %s", got, tc.want)
			}
			if got != strings.ToUpper(got) {
				t.Errorf("签名必须是大写十六进制: %s", got)
			}
		})
	}
}

// TestAimeSignatureDeterministic 断言同一输入总得到同一签名，且三要素任一变化都会改变签名。
func TestAimeSignatureDeterministic(t *testing.T) {
	base := aimeSignature("A63E-01C28055905", "250101120000", AimeCommonKey)

	if again := aimeSignature("A63E-01C28055905", "250101120000", AimeCommonKey); again != base {
		t.Error("同一输入应当得到同一签名")
	}
	if other := aimeSignature("A63E-01C28055905", "250101120001", AimeCommonKey); other == base {
		t.Error("时间戳变化应当改变签名")
	}
	if other := aimeSignature("A63E-01E68606624", "250101120000", AimeCommonKey); other == base {
		t.Error("chipID 变化应当改变签名")
	}
	if other := aimeSignature("A63E-01C28055905", "250101120000", "wrong-key"); other == base {
		t.Error("commonKey 变化应当改变签名")
	}
}

// newFakeAimeServer 启动假 AimeDB，记录收到的请求体。
func newFakeAimeServer(t *testing.T, respond func(body aimeRequest) (int, string)) (*AimeClient, *[]aimeRequest, *atomic.Int64) {
	t.Helper()

	var received []aimeRequest
	var calls atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("User-Agent"); got != AimeUserAgent {
			t.Errorf("User-Agent = %q, 期望 %q", got, AimeUserAgent)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, 期望 application/json", got)
		}

		var req aimeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体失败: %v", err)
			w.WriteHeader(http.StatusOK)
			return
		}
		received = append(received, req)

		status, body := respond(req)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	client, err := NewAimeClient(AimeOptions{
		HTTP:   newTestTransport(t),
		URL:    server.URL,
		Logger: nil,
		Now: func() time.Time {
			return time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)
		},
	})
	if err != nil {
		t.Fatalf("构造 AimeClient 失败: %v", err)
	}
	return client, &received, &calls
}

// freshSGID 构造一枚「刚签发」的假二维码。
func freshSGID(t *testing.T) SGID {
	t.Helper()
	sgid, err := NewSGID(buildSGID(time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)))
	if err != nil {
		t.Fatalf("构造假二维码失败: %v", err)
	}
	return sgid
}

// TestAimeExchangeRequestShape 断言请求体的字段名、时间戳格式，以及只发送 64 位片段。
//
// 只发后 64 位是硬性约束：把整串二维码发出去就等于把凭证多暴露一次。
func TestAimeExchangeRequestShape(t *testing.T) {
	client, received, _ := newFakeAimeServer(t, func(aimeRequest) (int, string) {
		return http.StatusOK, `{"errorID":0,"userID":10807675,"token":"token-abc"}`
	})

	cred, err := client.Exchange(context.Background(), freshSGID(t))
	if err != nil {
		t.Fatalf("Exchange 报错: %v", err)
	}
	if cred.UserID != 10807675 || cred.Token != "token-abc" {
		t.Errorf("凭据解析不符: %+v", cred)
	}

	if len(*received) != 1 {
		t.Fatalf("请求数 = %d, 期望 1", len(*received))
	}
	req := (*received)[0]

	if req.ChipID != AimeChipID {
		t.Errorf("chipID = %q, 期望 %q", req.ChipID, AimeChipID)
	}
	if req.OpenGameID != AimeOpenGameID {
		t.Errorf("openGameID = %q, 期望 %q", req.OpenGameID, AimeOpenGameID)
	}
	if req.QRCode != fakeTail {
		t.Errorf("qrCode = %q, 期望只是 64 位片段 %q", req.QRCode, fakeTail)
	}
	if strings.Contains(req.QRCode, "SGWCMAID") {
		t.Error("qrCode 不应包含二维码前缀")
	}
	if req.Timestamp != "260920205100" {
		t.Errorf("timestamp = %q, 期望 YYMMDDHHMMSS 格式 260920205100", req.Timestamp)
	}
	if want := aimeSignature(AimeChipID, "260920205100", AimeCommonKey); req.Key != want {
		t.Errorf("key = %q, 期望 %q", req.Key, want)
	}
}

// TestAimeExchangeSerializesCompactFieldNames 断言请求体的键名与文档逐字符一致。
func TestAimeExchangeSerializesCompactFieldNames(t *testing.T) {
	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = make([]byte, r.ContentLength)
		if _, err := r.Body.Read(raw); err != nil && len(raw) == 0 {
			t.Errorf("读取请求体失败: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errorID":0,"userID":1,"token":"t"}`))
	}))
	t.Cleanup(server.Close)

	client, err := NewAimeClient(AimeOptions{
		HTTP: newTestTransport(t),
		URL:  server.URL,
		// 固定当前时刻，让假二维码始终处于有效期内。
		Now: func() time.Time { return time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local) },
	})
	if err != nil {
		t.Fatalf("构造 AimeClient 失败: %v", err)
	}
	if _, err := client.Exchange(context.Background(), freshSGID(t)); err != nil {
		t.Fatalf("Exchange 报错: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	want := []string{"chipID", "openGameID", "key", "qrCode", "timestamp"}
	if len(parsed) != len(want) {
		t.Errorf("请求体字段数 = %d, 期望 %d（%v）", len(parsed), len(want), want)
	}
	for _, key := range want {
		if _, ok := parsed[key]; !ok {
			t.Errorf("请求体缺少字段 %q", key)
		}
	}
	// 紧凑 JSON：不应出现分隔用的空格。
	if strings.Contains(string(raw), `": `) {
		t.Errorf("请求体不是紧凑 JSON: %s", raw)
	}
}

// TestAimeExchangeErrorIDs 断言 errorID 被映射成带补救建议的错误。
func TestAimeExchangeErrorIDs(t *testing.T) {
	tests := []struct {
		name      string
		errorID   int
		wantErr   error
		wantClass Kind
	}{
		{"二维码过期 30 分钟档", 1, ErrAimeQRRejected, KindParam},
		{"二维码过期 10 分钟档", 2, ErrAimeQRRejected, KindParam},
		{"签名错误", 50, ErrAimeBadSignature, KindParam},
		{"未知错误码", 77, nil, KindBusiness},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := newFakeAimeServer(t, func(aimeRequest) (int, string) {
				return http.StatusOK, `{"errorID":` + strconv.Itoa(tc.errorID) + `}`
			})

			_, err := client.Exchange(context.Background(), freshSGID(t))
			if err == nil {
				t.Fatal("非零 errorID 应当报错")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("错误 = %v, 期望匹配 %v", err, tc.wantErr)
			}
			if !IsKind(err, tc.wantClass) {
				t.Errorf("错误分类不符: %v", err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.errorID)) {
				t.Errorf("错误信息应带上原始码: %v", err)
			}
		})
	}
}

// TestAimeExchangeExpiredSGIDSkipsRequest 断言过期二维码在本地就被拦下，不发请求。
//
// 少一次无效请求就少一分触发风控的机会。
func TestAimeExchangeExpiredSGIDSkipsRequest(t *testing.T) {
	client, _, calls := newFakeAimeServer(t, func(aimeRequest) (int, string) {
		return http.StatusOK, `{"errorID":1}`
	})

	expired, err := NewSGID(buildSGID(time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)))
	if err != nil {
		t.Fatalf("构造过期二维码失败: %v", err)
	}

	_, err = client.Exchange(context.Background(), expired)
	if !errors.Is(err, ErrSGIDExpired) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrSGIDExpired)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("过期二维码不应发出请求，实际发出 %d 次", got)
	}
}

// TestAimeExchangeRejectsInvalidSGID 断言非法二维码在本地被拦下。
func TestAimeExchangeRejectsInvalidSGID(t *testing.T) {
	client, _, calls := newFakeAimeServer(t, func(aimeRequest) (int, string) {
		return http.StatusOK, `{"errorID":0,"userID":1,"token":"t"}`
	})

	_, err := client.Exchange(context.Background(), SGID("garbage"))
	if !errors.Is(err, ErrSGIDFormat) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrSGIDFormat)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("非法二维码不应发出请求，实际发出 %d 次", got)
	}
}

// TestAimeExchangeEmptyBody 断言空响应被识别为 AimeDB 不可用。
func TestAimeExchangeEmptyBody(t *testing.T) {
	client, _, _ := newFakeAimeServer(t, func(aimeRequest) (int, string) {
		return http.StatusOK, ""
	})

	_, err := client.Exchange(context.Background(), freshSGID(t))
	if !errors.Is(err, ErrAimeUnavailable) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrAimeUnavailable)
	}
}

// TestAimeExchangeUnparsableBody 断言非 JSON 响应被识别为 AimeDB 不可用。
func TestAimeExchangeUnparsableBody(t *testing.T) {
	client, _, _ := newFakeAimeServer(t, func(aimeRequest) (int, string) {
		return http.StatusOK, "<html>网关拦截</html>"
	})

	_, err := client.Exchange(context.Background(), freshSGID(t))
	if !errors.Is(err, ErrAimeUnavailable) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrAimeUnavailable)
	}
}

// TestAimeExchangeIncompleteSuccess 断言 errorID=0 但缺字段时按不可用处理。
func TestAimeExchangeIncompleteSuccess(t *testing.T) {
	client, _, _ := newFakeAimeServer(t, func(aimeRequest) (int, string) {
		return http.StatusOK, `{"errorID":0,"userID":0,"token":""}`
	})

	_, err := client.Exchange(context.Background(), freshSGID(t))
	if err == nil {
		t.Fatal("缺 userId 与 token 时应当报错")
	}
}

// TestCredentialNeverRevealsToken 断言凭证不会通过 String / JSON / 日志泄露 token。
func TestCredentialNeverRevealsToken(t *testing.T) {
	cred := Credential{UserID: 10807675, Token: "super-secret-token"}

	if strings.Contains(cred.String(), "super-secret-token") {
		t.Errorf("String() 泄露了 token: %s", cred.String())
	}
	if !strings.Contains(cred.String(), "10807675") {
		t.Errorf("String() 应保留 userId: %s", cred.String())
	}

	encoded, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("MarshalJSON 报错: %v", err)
	}
	if strings.Contains(string(encoded), "super-secret-token") {
		t.Errorf("JSON 序列化泄露了 token: %s", encoded)
	}

	if got := cred.LogValue().String(); strings.Contains(got, "super-secret-token") {
		t.Errorf("日志值泄露了 token: %s", got)
	}
}

// TestCredentialValidate 断言凭证完整性校验。
func TestCredentialValidate(t *testing.T) {
	tests := []struct {
		name    string
		cred    Credential
		wantErr bool
	}{
		{"完整", Credential{UserID: 1, Token: "t"}, false},
		{"缺 userId", Credential{UserID: 0, Token: "t"}, true},
		{"缺 token", Credential{UserID: 1}, true},
		{"全空", Credential{}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cred.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, 期望出错 = %v", err, tc.wantErr)
			}
		})
	}
}
