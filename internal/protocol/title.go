package protocol

import (
	"context"
	"crypto/aes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// primeSession 用一次 Preview 换取会话 Cookie。
//
// 失败不阻断登录流程：真正的凭证问题会在 Login 的返回码里暴露，
// 这里只负责把 JSESSIONID 拿到手（服务端在 Preview 的响应头里下发）。
func (c *TitleClient) primeSession(ctx context.Context) {
	req := userPreviewRequest{
		UserID:   c.agentID,
		Token:    c.cred.Token,
		ClientID: c.version.Login.ClientID,
	}
	var preview userPreviewResponse
	if err := c.call(ctx, APIGetUserPreview, req, &preview); err != nil {
		if c.logger != nil {
			c.logger.Warn("Preview 预热失败，会话 Cookie 可能缺失", "原因", err.Error())
		}
		return
	}
	if c.logger != nil {
		c.logger.Debug("会话已预热", "cookie", c.cookie != "")
	}
}

// absorbSession 记下服务端下发的会话 cookie，供后续请求回传。
//
// 1.53+ 的会话状态不只在请求体：登录成功后服务端会用 Set-Cookie 下发会话标识，
// 不带上它的后续请求会被判为「无有效会话」并返回空体——症状与版本错、被阻断都很像。
func (c *TitleClient) absorbSession(resp *transport.Response) {
	for _, raw := range resp.Header.Values("Set-Cookie") {
		pair, _, _ := strings.Cut(raw, ";")
		if pair = strings.TrimSpace(pair); pair != "" {
			c.cookie = pair
			return
		}
	}
}

// waitTurn 按服务端要求的间隔节流，并在 ctx 取消时立即返回。
//
// 服务端对超频请求返回空体而非错误码，不节流的话后续步骤会接连「查无数据」。
func (c *TitleClient) waitTurn(ctx context.Context) error {
	if !c.lastCall.IsZero() {
		if wait := DefaultRequestInterval - time.Since(c.lastCall); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	c.lastCall = time.Now()
	return nil
}

// plaintextPeek 在响应不可能是什么 AES 密文时返回其可读开头。
//
// 服务端抛异常时返回的是明文错误页（HTTP 500 + Tomcat HTML），直接拿去解密只会报
// 「密文长度不是 16 的倍数」，把排查方向误导到协议版本上——实测就被这么带偏过一次。
func plaintextPeek(body []byte) string {
	if len(body) == 0 || len(body)%aes.BlockSize == 0 {
		return ""
	}
	const peekLen = 180
	if len(body) > peekLen {
		body = body[:peekLen]
	}
	return strings.TrimSpace(string(body))
}

// pingRequest 是探活请求体。
type pingRequest struct {
	Ping int `json:"ping"`
}

// maxMusicPages 是分页拉取的硬上限，防止服务端异常导致无限循环。
const maxMusicPages = 64

// TitleOptions 是构造 TitleClient 的全部依赖。
type TitleOptions struct {
	// HTTP 是传输客户端，必填。
	HTTP *transport.Client

	// Version 是已解析的协议参数，必填。
	Version Version

	// BaseURL 是标题服务器根地址，为空取 DefaultTitleBaseURL。
	BaseURL string

	// Logger 用于记录脱敏后的步骤日志，nil 表示不记录。
	Logger *slog.Logger
}

// TitleClient 访问机台标题服务器，承担登录、取分、登出与资料拉取。
//
// 同一实例对应一个账号的一次会话：Login 成功后它会记住 dateTime，
// Logout 必须使用同一个 dateTime，否则服务端不认，会话会残留到 15 分钟硬超时。
type TitleClient struct {
	hc      *transport.Client
	version Version
	baseURL string
	logger  *slog.Logger

	agentID  int
	dateTime int64
	loggedIn bool
	lastCall time.Time
	cookie   string
	cred     Credential
}

// NewTitleClient 构造 TitleClient。
func NewTitleClient(opts TitleOptions) (*TitleClient, error) {
	if opts.HTTP == nil {
		return nil, fmt.Errorf("%w: TitleClient 需要 transport.Client", ErrParam)
	}
	if err := opts.Version.Validate(); err != nil {
		return nil, err
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultTitleBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return &TitleClient{
		hc:      opts.HTTP,
		version: opts.Version,
		baseURL: base,
		logger:  opts.Logger,
	}, nil
}

// Version 返回本客户端使用的协议版本。
func (c *TitleClient) Version() Version { return c.version }

// HasSession 报告是否已建立登录会话。
func (c *TitleClient) HasSession() bool { return c.loggedIn }

// nowMillis 按版本要求给出时间戳：1.55 用秒，更早的版本用毫秒。
//
// 单位写错时服务端会直接拒绝登录，且报的是 HTTP 500，看不出跟时间单位有关。
func (c *TitleClient) nowMillis() int64 {
	if c.version.Login.TimestampSeconds {
		return time.Now().Unix()
	}
	return time.Now().UnixMilli()
}

// Ping 探活：不需要凭证，用于在拉成绩前判定连通性与协议参数是否匹配。
func (c *TitleClient) Ping(ctx context.Context) error {
	var raw json.RawMessage
	if err := c.call(ctx, APIPing, pingRequest{Ping: 1}, &raw); err != nil {
		return err
	}
	// 响应结构随版本漂移，这里只在能读到 returnCode 时才校验，避免因字段变动误报。
	var probe struct {
		ReturnCode *int `json:"returnCode"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	if probe.ReturnCode != nil && *probe.ReturnCode != 1 {
		return titleReturnCodeError(APIPing, *probe.ReturnCode)
	}
	return nil
}

// Login 用 AimeDB 凭证建立会话。
//
// 成功后本客户端记下 dateTime 供 Logout 使用；返回码 100 表示账号已在登录状态，
// 此时不会建立会话，也不应再尝试登出。
func (c *TitleClient) Login(ctx context.Context, cred Credential, regionID int) error {
	if err := cred.Validate(); err != nil {
		return err
	}
	c.agentID = cred.UserID
	c.cred = cred

	// 登录前先调一次 Preview：服务端在它的响应里下发 JSESSIONID，
	// 缺这个 Cookie 时后续所有请求都会返回空体（症状与版本错、被阻断难以区分）。
	c.primeSession(ctx)

	c.dateTime = c.nowMillis()

	if regionID == 0 {
		regionID = c.version.Login.RegionID
	}
	req := userLoginRequest{
		UserID:        cred.UserID,
		RegionID:      regionID,
		DateTime:      c.dateTime,
		LoginDateTime: c.dateTime,
		AcsessCode:    "",
		PlaceID:       c.version.Login.PlaceID,
		ClientID:      c.version.Login.ClientID,
		Token:         cred.Token,
		IsContinue:    c.version.Login.ContinueOnLogin,
		GenericFlag:   0,
		shape:         c.version.Login,
	}

	var resp userLoginResponse
	if err := c.call(ctx, APIUserLogin, req, &resp); err != nil {
		return err
	}
	if resp.ReturnCode != 1 {
		if resp.ReturnCode == 100 {
			return wrap(ErrAlreadyLoggedIn, "returnCode=100", nil)
		}
		return titleReturnCodeError(APIUserLogin, resp.ReturnCode)
	}

	c.loggedIn = true
	if c.logger != nil {
		c.logger.Info("登录成功", "userId", cred.UserID, "版本", c.version.Encoding)
	}
	return nil
}

// Logout 结束会话，必须使用与登录相同的 dateTime。
//
// 未建立会话时它直接返回：此时盲目登出会带上错误的 loginDateTime，
// 既不能清掉别人的会话，又可能把自己拖进「小黑屋」。
func (c *TitleClient) Logout(ctx context.Context) error {
	if !c.loggedIn {
		if c.logger != nil {
			c.logger.Debug("跳过登出：本次未建立会话")
		}
		return nil
	}

	req := userLogoutRequest{
		UserID:        c.agentID,
		LoginDateTime: c.dateTime,
		RegionID:      c.version.Login.RegionID,
		PlaceID:       c.version.Login.PlaceID,
		ClientID:      c.version.Login.ClientID,
		AcsessCode:    "",
		shape:         c.version.Login,
	}
	var resp userLogoutResponse
	err := c.call(ctx, APIUserLogout, req, &resp)
	if err != nil {
		return err
	}
	if resp.ReturnCode != 1 && resp.ReturnCode != 0 {
		return titleReturnCodeError(APIUserLogout, resp.ReturnCode)
	}

	c.loggedIn = false
	if c.logger != nil {
		c.logger.Info("登出完成", "userId", c.agentID)
	}
	return nil
}

// Music 分页拉取全量成绩，并过滤掉未游玩（playCount<=0）的条目。
func (c *TitleClient) Music(ctx context.Context) ([]model.Score, error) {
	if !c.loggedIn {
		return nil, fmt.Errorf("%w: 拉取成绩前必须先登录", ErrParam)
	}

	var all []model.Score
	nextIndex := 0
	for page := 0; page < maxMusicPages; page++ {
		req := getUserMusicRequest{UserID: c.agentID, NextIndex: nextIndex, MaxCount: musicPageSize}

		var resp getUserMusicResponse
		if err := c.call(ctx, APIGetUserMusic, req, &resp); err != nil {
			return nil, err
		}
		all = append(all, scoresFromMusic(resp)...)

		if resp.NextIndex == 0 || len(resp.UserMusicList) == 0 {
			if c.logger != nil {
				c.logger.Info("成绩拉取完成", "条数", len(all), "轮次", page+1)
			}
			return all, nil
		}
		if resp.NextIndex == nextIndex {
			return nil, businessErr("分页游标未推进，可能服务端异常", fmt.Sprintf("nextIndex=%d", nextIndex), "稍后重试；若持续出现请保留日志")
		}
		nextIndex = resp.NextIndex
	}
	return nil, businessErr("成绩分页超出上限仍未结束", fmt.Sprintf("轮次=%d", maxMusicPages), "确认 maxCount 是否被服务端限流")
}

// Profile 拉取账号资料：头像、姓名框、牌子、称号与评级。
func (c *TitleClient) Profile(ctx context.Context) (Profile, error) {
	if !c.loggedIn {
		return Profile{}, fmt.Errorf("%w: 拉取资料前必须先登录", ErrParam)
	}

	// 两个接口互补，任一失败都退回零值继续，避免因单个字段缺失让整个命令失败。
	var preview userPreviewResponse
	if err := c.call(ctx, APIGetUserPreview, userPreviewRequest{
		UserID:   c.agentID,
		Token:    c.cred.Token,
		ClientID: c.version.Login.ClientID,
	}, &preview); err != nil && c.logger != nil {
		c.logger.Warn("GetUserPreviewApi 失败，资料字段将不完整", "原因", err.Error())
	}
	var data getUserDataResponse
	if err := c.call(ctx, APIGetUserData, getUserDataRequest{UserID: c.agentID}, &data); err != nil && c.logger != nil {
		c.logger.Warn("GetUserDataApi 失败，评级字段将不完整", "原因", err.Error())
	}
	return mergeProfile(preview, data), nil
}

// call 执行一次机台 API 调用：算 hash、打包、发送、解包、反序列化。
//
// 四类故障在这里被区分开：传输失败（网络）、响应 0 字节（被阻断）、
// 解密或反序列化失败（版本不匹配）、解析出业务错误码（业务问题）。
func (c *TitleClient) call(ctx context.Context, apiName string, reqBody any, out any) error {
	if err := c.waitTurn(ctx); err != nil {
		return err
	}

	hash := apiHash(apiName, c.version.ObfuscateParam)
	body, err := packBody(reqBody, c.version)
	if err != nil {
		return err
	}

	headers := map[string]string{
		// agent_id 就是 userId；未登录时（Ping）为 0。
		"User-Agent": fmt.Sprintf("%s#%d", hash, c.agentID),
		// 参考实现会带上它，服务端可能据此区分请求来源。
		"number":           "0",
		"Content-Type":     "application/json",
		"Mai-Encoding":     c.version.Encoding,
		"Charset":          "UTF-8",
		"Content-Encoding": "deflate",
		// 显式置空以禁用压缩协商：机台要求原样收发。
		"Accept-Encoding": "",
		"Expect":          "100-continue",
	}
	if c.cookie != "" {
		headers["Cookie"] = c.cookie
	}

	if c.logger != nil {
		c.logger.Debug("机台请求",
			"api", apiName,
			"hash", hash,
			"版本", c.version.Encoding,
			"请求字节", len(body))
	}

	resp, err := c.hc.Post(ctx, c.baseURL+hash, headers, body)
	if err != nil {
		return classifyTransport(err)
	}
	c.absorbSession(resp)
	if resp.ErrEmpty() {
		return wrap(ErrEmptyResponse, fmt.Sprintf("HTTP %d · %s", resp.StatusCode, apiName), nil)
	}

	plain, err := unpackBody(resp.Body, c.version)
	if err != nil {
		if peek := plaintextPeek(resp.Body); peek != "" {
			return wrap(ErrServerPlaintext,
				fmt.Sprintf("HTTP %d · %s · %s", resp.StatusCode, apiName, peek), err)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(plain, out); err != nil {
		return wrap(ErrVersionMismatch,
			fmt.Sprintf("HTTP %d · %d 字节 · %s", resp.StatusCode, len(resp.Body), apiName), err)
	}
	return nil
}

// titleReturnCodeError 把 returnCode 翻译成带原因与建议的业务错误。
func titleReturnCodeError(apiName string, returnCode int) error {
	e := &Error{Kind: KindBusiness, Code: fmt.Sprintf("%s returnCode=%d", apiName, returnCode)}
	if known, ok := titleReturnCodeReasons[returnCode]; ok {
		e.Msg = known.Reason
		e.Hint = known.Hint
	} else {
		e.Msg = "机台返回未知 returnCode"
		e.Hint = "确认协议版本与账号状态；必要时保留日志排查"
	}
	return e
}
