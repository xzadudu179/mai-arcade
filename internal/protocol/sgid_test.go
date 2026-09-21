package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeTail 是 64 位十六进制占位符，用于构造格式合法的假二维码。
const fakeTail = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"

// buildSGID 按给定签发时间拼出格式合法的二维码原文。
func buildSGID(issued time.Time) string {
	return sgidPrefix + issued.Format(sgidTimestampLayout) + fakeTail
}

// TestNewSGIDAcceptsValidForms 断言纯文本与公众号链接都能被接受，并归一化成同一结果。
func TestNewSGIDAcceptsValidForms(t *testing.T) {
	issued := time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)
	plain := buildSGID(issued)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"纯文本", plain, plain},
		{"req 链接需补回 SGWC 前缀", "https://wq.wahlap.net/qrcode/req/MAID" + issued.Format(sgidTimestampLayout) + fakeTail, plain},
		{"img 链接需补回 SGWC 前缀", "https://wq.wahlap.net/qrcode/img/MAID" + issued.Format(sgidTimestampLayout) + fakeTail, plain},
		{"链接带查询串", "https://wq.wahlap.net/qrcode/req/MAID" + issued.Format(sgidTimestampLayout) + fakeTail + "?t=1", plain},
		{"链接带图片后缀", "https://wq.wahlap.net/qrcode/img/MAID" + issued.Format(sgidTimestampLayout) + fakeTail + ".png", plain},
		{"前后空白需去除", "  \n " + plain + "\t ", plain},
		{"小写需归一化为大写", strings.ToLower(plain), plain},
		{"链接小写", strings.ToLower("https://wq.wahlap.net/qrcode/req/MAID" + issued.Format(sgidTimestampLayout) + fakeTail), plain},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSGID(tc.input)
			if err != nil {
				t.Fatalf("NewSGID 报错: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("得到 %q, 期望 %q", got, tc.want)
			}
			if !got.Valid() {
				t.Error("解析结果应当自认为合法")
			}
			if got.qrCode() != fakeTail {
				t.Errorf("qrCode() = %q, 期望 %q", got.qrCode(), fakeTail)
			}
		})
	}
}

// TestNewSGIDRejectsInvalid 断言非法输入一律被拒。
//
// 二维码是账号唯一凭证，格式校验是唯一入口守卫，任何漏网之鱼都会变成一次无效请求。
func TestNewSGIDRejectsInvalid(t *testing.T) {
	issued := time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)
	valid := buildSGID(issued)

	tests := []struct {
		name  string
		input string
	}{
		{"空输入", ""},
		{"只有空白", "   "},
		{"长度少一位", valid[:83]},
		{"长度多一位", valid + "A"},
		{"前缀错误", "SGWCXAID" + valid[8:]},
		{"缺少前缀", valid[4:]},
		{"有效段含非十六进制", valid[:20] + "Z" + valid[21:]},
		{"时间戳含非数字", valid[:8] + "20AB09202051" + valid[20:]},
		{"链接里找不到 MAID 段", "https://wq.wahlap.net/qrcode/req/XXXX123"},
		{"完全无关的文本", "not-a-qrcode"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSGID(tc.input)
			if err == nil {
				t.Fatalf("非法输入被接受了: %q", got)
			}
			if !errors.Is(err, ErrSGIDFormat) {
				t.Errorf("错误应属于格式类，得到 %v", err)
			}
		})
	}
}

// TestSGIDExpiry 断言新鲜度判定：超 10 分钟判过期，未来时间容错。
func TestSGIDExpiry(t *testing.T) {
	now := time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)

	tests := []struct {
		name   string
		issued time.Time
		want   bool
	}{
		{"刚签发", now, false},
		{"8 分钟前仍在有效期内", now.Add(-8 * time.Minute), false},
		{"恰好 10 分钟视为未过期", now.Add(-SGIDValidity), false},
		{"11 分钟前已过期", now.Add(-11 * time.Minute), true},
		{"一小时前已过期", now.Add(-time.Hour), true},
		{"未来 5 分钟按时钟偏差容错", now.Add(5 * time.Minute), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sgid, err := NewSGID(buildSGID(tc.issued))
			if err != nil {
				t.Fatalf("构造假二维码失败: %v", err)
			}
			if got := sgid.Expired(now); got != tc.want {
				t.Errorf("Expired() = %v, 期望 %v", got, tc.want)
			}
			issuedAt, err := sgid.IssuedAt()
			if err != nil {
				t.Fatalf("IssuedAt 报错: %v", err)
			}
			if !issuedAt.Equal(tc.issued) {
				t.Errorf("IssuedAt() = %v, 期望 %v", issuedAt, tc.issued)
			}
		})
	}
}

// TestSGIDExpiredOnBrokenValue 断言零值或不合法值的过期判定是「已过期」。
//
// 保守方向才安全：把坏的当成好的会让请求真的发出去，白的当成坏的只是让用户重新扫码。
func TestSGIDExpiredOnBrokenValue(t *testing.T) {
	for _, sgid := range []SGID{"", "garbage", "SGWCMAID"} {
		if !sgid.Expired(time.Now()) {
			t.Errorf("%q 的 Expired() 应为 true", sgid)
		}
		if sgid.Valid() {
			t.Errorf("%q 不应被判为合法", sgid)
		}
	}
}

// TestSGIDNeverRevealsPlaintext 断言二维码不会通过 String / JSON / 日志泄露全文。
func TestSGIDNeverRevealsPlaintext(t *testing.T) {
	issued := time.Date(2026, 9, 20, 20, 51, 0, 0, time.Local)
	raw := buildSGID(issued)
	sgid, err := NewSGID(raw)
	if err != nil {
		t.Fatalf("构造假二维码失败: %v", err)
	}

	if strings.Contains(sgid.String(), fakeTail) {
		t.Errorf("String() 泄露了全文: %s", sgid.String())
	}
	if !strings.Contains(sgid.String(), "SGWCMAID") || !strings.Contains(sgid.String(), "84") {
		t.Errorf("String() 应给出前缀与长度: %s", sgid.String())
	}

	encoded, err := json.Marshal(sgid)
	if err != nil {
		t.Fatalf("MarshalJSON 报错: %v", err)
	}
	if strings.Contains(string(encoded), fakeTail) {
		t.Errorf("JSON 序列化泄露了全文: %s", encoded)
	}

	if got := sgid.LogValue().String(); strings.Contains(got, fakeTail) {
		t.Errorf("日志值泄露了全文: %s", got)
	}
}
