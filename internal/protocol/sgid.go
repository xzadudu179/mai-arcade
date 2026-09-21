package protocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// SGID 是已校验的 SGWCMAID 二维码内容。
//
// 零值不可用：唯一合法的构造途径是 NewSGID，它保证长度、前缀与十六进制片段都合规，
// 非法值因此无法进入系统内部。
//
// 安全约束：SGID 是 1.53+ 的账号唯一凭证，有效期内持有者可登录该账号。
// 它绝不落盘、绝不写日志、绝不回显——因此 String 与 MarshalJSON 都被刻意实现为脱敏输出，
// 即使有人误把它塞进日志或 JSON 输出，也不会泄露全文。
type SGID string

const (
	sgidPrefix          = "SGWCMAID"
	sgidLength          = 84
	sgidTimestampStart  = len(sgidPrefix)
	sgidTimestampLen    = 12
	sgidQRCodeStart     = sgidTimestampStart + sgidTimestampLen
	sgidTimestampLayout = "060102150405"
)

// SGIDValidity 是二维码的有效期；超过即需重新获取。
const SGIDValidity = 10 * time.Minute

// qrCodeURLPattern 匹配公众号二维码图片链接，捕获其中缺少 SGWC 前缀的部分。
//
// 链接形如 https://wq.wahlap.net/qrcode/req/MAID... 或 /qrcode/img/MAID...，
// 路径里的内容比完整二维码少了开头的 SGWC 四个字符。
// 大小写不敏感：链接可能整串来自小写复制的场景，归一化留到提取之后做。
var qrCodeURLPattern = regexp.MustCompile(`(?i)wq\.wahlap\.net/qrcode/(?:req|img)/(MAID[^?.\s]+)`)

// NewSGID 校验并归一化二维码内容。
//
// 接受两种输入：完整二维码原文，或公众号下发的图片链接（自动提取并补回 SGWC 前缀）。
// 十六进制片段大小写不限，统一转为大写。
func NewSGID(raw string) (SGID, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", fmt.Errorf("%w: 输入为空", ErrSGIDFormat)
	}

	// 先按原样提取链接里的内容，再统一转大写：先转大写会让小写域名匹配不上链接正则。
	if m := qrCodeURLPattern.FindStringSubmatch(candidate); m != nil {
		candidate = "SGWC" + m[1]
	}
	candidate = strings.ToUpper(candidate)

	if err := validateSGID(candidate); err != nil {
		return "", err
	}
	return SGID(candidate), nil
}

// validateSGID 检查长度、前缀与后 64 位的十六进制合法性。
func validateSGID(s string) error {
	if len(s) != sgidLength {
		return fmt.Errorf("%w: 长度 %d，应为 %d", ErrSGIDFormat, len(s), sgidLength)
	}
	if !strings.HasPrefix(s, sgidPrefix) {
		return fmt.Errorf("%w: 缺少 %s 前缀", ErrSGIDFormat, sgidPrefix)
	}
	for i := sgidQRCodeStart; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return fmt.Errorf("%w: 第 %d 位 %q 不是十六进制字符", ErrSGIDFormat, i+1, s[i])
		}
	}
	if _, err := parseSGIDTimestamp(s); err != nil {
		return err
	}
	return nil
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'F')
}

// IssuedAt 返回二维码的签发时间（按本地时区解释 12 位时间戳）。
func (s SGID) IssuedAt() (time.Time, error) { return parseSGIDTimestamp(string(s)) }

func parseSGIDTimestamp(s string) (time.Time, error) {
	if len(s) < sgidQRCodeStart {
		return time.Time{}, fmt.Errorf("%w: 内容过短，取不到时间戳", ErrSGIDFormat)
	}
	raw := s[sgidTimestampStart:sgidQRCodeStart]
	t, err := time.ParseInLocation(sgidTimestampLayout, raw, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: 时间戳 %q 无法解析: %v", ErrSGIDFormat, raw, err)
	}
	return t, nil
}

// Expired 报告二维码在 now 时刻是否已超过有效期。
//
// 时间戳晚于 now（时钟偏差或时区配置差异）按未过期处理：误判为过期会让用户白跑一趟，
// 而真正的过期由服务端返回的 errorID 兜底。
func (s SGID) Expired(now time.Time) bool {
	issued, err := s.IssuedAt()
	if err != nil {
		return true
	}
	if issued.After(now) {
		return false
	}
	return now.Sub(issued) > SGIDValidity
}

// Valid 报告是否为已校验的非零值。
func (s SGID) Valid() bool { return validateSGID(string(s)) == nil }

// qrCode 返回 AimeDB 需要的 64 位片段（不含前缀与时间戳）。
func (s SGID) qrCode() string {
	if len(s) < sgidQRCodeStart {
		return ""
	}
	return string(s[sgidQRCodeStart:])
}

// timestamp 返回 12 位时间戳字符串。
func (s SGID) timestamp() string {
	if len(s) < sgidQRCodeStart {
		return ""
	}
	return string(s[sgidTimestampStart:sgidQRCodeStart])
}

// String 返回脱敏表示：仅前 8 位与总长度。
func (s SGID) String() string {
	return fmt.Sprintf("%s…(len=%d)", s.prefix8(), len(s))
}

func (s SGID) prefix8() string {
	if len(s) < 8 {
		return string(s)
	}
	return string(s[:8])
}

// MarshalJSON 输出脱敏表示而非全文，杜绝二维码被序列化进日志或结构化输出。
func (s SGID) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// LogValue 让 slog 记录时自动脱敏。
func (s SGID) LogValue() slog.Value { return slog.StringValue(s.String()) }
