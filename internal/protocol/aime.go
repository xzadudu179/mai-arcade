package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// aimeTimestampLayout 是 AimeDB 要求的时间戳格式，按本地时区解释。
const aimeTimestampLayout = "060102150405"

// Credential 是 AimeDB 换来的机台账号凭证。
//
// 安全约束：Token 是账号凭证，绝不写日志、绝不回显。String 与 MarshalJSON 都只输出脱敏表示。
type Credential struct {
	UserID int
	Token  string

	// Key 与 Timestamp 是 AimeDB 随 token 一起下发的签名材料（实测存在，文档未记录）。
	//
	// 当前登录流程用不到它们，但服务端既然给了就保留下来：
	// 1.53+ 的「token 强校验」可能依赖这组值，将来排查时可避免重新走一遍换账号。
	Key       string
	Timestamp string
}

// String 返回脱敏表示：userId 明文 + token 前 4 位。
func (c Credential) String() string {
	return fmt.Sprintf("userId=%d token=%s", c.UserID, transport.Redact(c.Token, 4))
}

// LogValue 让 slog 记录时自动脱敏。
func (c Credential) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON 输出脱敏表示而非 token 全文。
func (c Credential) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// Validate 报告凭证是否可用。
func (c Credential) Validate() error {
	if c.UserID <= 0 {
		return fmt.Errorf("%w: userId 缺失", ErrParam)
	}
	if c.Token == "" {
		return fmt.Errorf("%w: token 缺失", ErrParam)
	}
	return nil
}

// AimeOptions 是构造 AimeClient 的全部依赖。
type AimeOptions struct {
	// HTTP 是传输客户端，必填。
	HTTP *transport.Client

	// URL 是接口地址，为空取 AimeDBURL。
	URL string

	// ChipID 是机台标识，为空取 AimeChipID（1.53 起）。
	ChipID string

	// CommonKey 是签名密钥，为空取 AimeCommonKey。
	CommonKey string

	// Now 提供当前时间，便于测试注入固定时刻；为空取 time.Now。
	Now func() time.Time

	// Logger 用于记录脱敏后的步骤日志，nil 表示不记录。
	Logger *slog.Logger
}

// AimeClient 通过二维码在 AimeDB 换取机台账号凭证。
type AimeClient struct {
	hc        *transport.Client
	url       string
	chipID    string
	commonKey string
	now       func() time.Time
	logger    *slog.Logger
}

// NewAimeClient 构造 AimeClient，缺省字段填入文档规定的默认值。
func NewAimeClient(opts AimeOptions) (*AimeClient, error) {
	if opts.HTTP == nil {
		return nil, fmt.Errorf("%w: AimeClient 需要 transport.Client", ErrParam)
	}
	c := &AimeClient{
		hc:        opts.HTTP,
		url:       opts.URL,
		chipID:    opts.ChipID,
		commonKey: opts.CommonKey,
		now:       opts.Now,
		logger:    opts.Logger,
	}
	if c.url == "" {
		c.url = AimeDBURL
	}
	if c.chipID == "" {
		c.chipID = AimeChipID
	}
	if c.commonKey == "" {
		c.commonKey = AimeCommonKey
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// Exchange 用二维码换取 userId 与 token。
//
// 只发送二维码的 64 位片段；过期与签名错误会被翻译成带补救建议的错误。
func (c *AimeClient) Exchange(ctx context.Context, sgid SGID) (Credential, error) {
	if !sgid.Valid() {
		return Credential{}, fmt.Errorf("%w: 二维码未通过校验", ErrSGIDFormat)
	}
	if sgid.Expired(c.now()) {
		return Credential{}, ErrSGIDExpired
	}

	timestamp := c.now().Format(aimeTimestampLayout)
	req := aimeRequest{
		ChipID:     c.chipID,
		OpenGameID: AimeOpenGameID,
		Key:        aimeSignature(c.chipID, timestamp, c.commonKey),
		QRCode:     sgid.qrCode(),
		Timestamp:  timestamp,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Credential{}, fmt.Errorf("序列化 AimeDB 请求: %w", err)
	}

	if c.logger != nil {
		c.logger.Debug("aime 请求",
			"时间戳", timestamp,
			"chipID", c.chipID,
			"二维码", sgid.String(),
			"请求字节", len(body))
	}

	headers := map[string]string{
		"User-Agent":   AimeUserAgent,
		"Content-Type": "application/json",
	}
	resp, err := c.hc.Post(ctx, c.url, headers, body)
	if err != nil {
		return Credential{}, classifyTransport(err)
	}
	if resp.ErrEmpty() {
		return Credential{}, wrap(ErrAimeUnavailable, fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
	}

	var parsed aimeResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return Credential{}, wrap(ErrAimeUnavailable, "60002",
			fmt.Errorf("响应 %d 字节无法解析为 JSON: %w", len(resp.Body), err))
	}
	if parsed.ErrorID != 0 {
		return Credential{}, aimeError(parsed.ErrorID)
	}

	cred := Credential{UserID: parsed.UserID, Token: parsed.Token, Key: parsed.Key, Timestamp: parsed.Timestamp}
	if err := cred.Validate(); err != nil {
		return Credential{}, wrap(ErrAimeUnavailable, fmt.Sprintf("errorID=0 但响应不完整 HTTP %d", resp.StatusCode), err)
	}
	return cred, nil
}

// aimeSignature 计算 AimeDB 的 key：sha256(chipID + timestamp + commonKey) 的十六进制大写。
func aimeSignature(chipID, timestamp, commonKey string) string {
	sum := sha256.Sum256([]byte(chipID + timestamp + commonKey))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// aimeError 把 errorID 翻译成带原因与建议的错误。
//
// errorID 1/2 与 50 必须区分开：前者说明请求格式与签名都被服务端接受了（只是二维码不可用），
// 后者说明签名本身不对。probe 依赖这个区别做无凭证自检。
func aimeError(errorID int) error {
	switch errorID {
	case 1, 2:
		return wrap(ErrAimeQRRejected, fmt.Sprintf("errorID=%d", errorID), nil)
	case 50:
		return wrap(ErrAimeBadSignature, fmt.Sprintf("errorID=%d", errorID), nil)
	}

	e := &Error{Kind: KindBusiness, Code: fmt.Sprintf("errorID=%d", errorID)}
	if known, ok := aimeErrorReasons[errorID]; ok {
		e.Msg = known.Reason
		e.Hint = known.Hint
	} else {
		e.Msg = "AimeDB 返回未知错误码"
		e.Hint = "确认二维码来自官方公众号且未被使用过"
	}
	return e
}

// classifyTransport 把传输层错误归类到协议层错误。
func classifyTransport(err error) error {
	if isTransportTimeout(err) {
		return wrap(ErrTimeout, "", err)
	}
	return wrap(ErrNetwork, "", err)
}

func isTransportTimeout(err error) bool {
	return errors.Is(err, transport.ErrTimeout)
}
