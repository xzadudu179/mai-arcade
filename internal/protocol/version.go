// Package protocol 实现舞萌 DX 机台侧协议：二维码解析、AimeDB 换账号、标题服务器的加解密与调用。
//
// 它只懂机台协议，不懂任何查分器；产出归一化的 model.Score 交给上层。
package protocol

import "fmt"

// LoginShape 描述登录与登出请求的字段差异。
//
// 这些差异只能以数据形式存在：不同版本用不同的字段名与时间单位，
// 写错会被服务端直接拒绝（实测表现为 HTTP 500 的 Tomcat 错误页，而不是业务返回码）。
type LoginShape struct {
	// AccessCodeField 是「访问码」字段的键名。
	//
	// 1.55 用 accessCode；更早的官方源码把拼写错成了 acsessCode，两种都被服务端接受过，
	// 抄错的那一份会被判为缺字段。
	AccessCodeField string

	// TimestampSeconds 为真时 dateTime 用秒，否则用毫秒。
	TimestampSeconds bool

	// IncludeLoginDateTime 为真时登录请求里同时带 loginDateTime（与 dateTime 同值）。
	IncludeLoginDateTime bool

	// ContinueOnLogin 是 isContinue 字段的取值。
	ContinueOnLogin bool

	// RegionID 是登录/登出请求里的 regionId。
	RegionID int

	// LogoutType 是登出请求的 type 字段；为 0 表示该版本不发这个字段。
	LogoutType int

	// ClientID 是机台客户端标识：keychip 去掉横线后的前 11 位。
	//
	// 服务端用它匹配机台会话，传空会被判为非法请求——实测登录返回 HTTP 500。
	ClientID string

	// PlaceID 是门店编号：服务端会把它记进登录会话，传空同样触发 HTTP 500。
	PlaceID string
}

// Version 是一套机台协议参数。版本差异只以数据形式存在——业务代码里不允许出现按版本号分支的判断。
type Version struct {
	// Encoding 是 Mai-Encoding 请求头的取值，也是本版本的标识，如 "1.53"。
	Encoding string

	// ObfuscateParam 是 api_hash 计算中的混淆参数。
	ObfuscateParam string

	// Key 是 AES-256 密钥，必须 32 字节。
	Key []byte

	// IV 是 AES-CBC 初始向量，必须 16 字节。
	IV []byte

	// ZlibBeforeEncrypt 标记压缩与加密的先后：1.53 起先压缩后加密，1.40–1.52 相反。
	ZlibBeforeEncrypt bool

	// Login 描述登录与登出请求的字段形状。
	Login LoginShape
}

// legacyLoginShape 是 1.53 及更早版本的登录字段形状。
//
// 来自原始规范 §2.5 的记录：字段名是官方源码里的拼写错误 acsessCode，dateTime 用毫秒。
var legacyLoginShape = LoginShape{
	AccessCodeField:      "acsessCode",
	TimestampSeconds:     false,
	IncludeLoginDateTime: false,
	ContinueOnLogin:      true,
	RegionID:             0,
	LogoutType:           0,
	ClientID:             DefaultClientID,
	PlaceID:              DefaultPlaceID,
}

// modernLoginShape 是 1.55 的登录字段形状。
//
// 来自社区参考实现（MAI_ENCODING=1.55 的那一份）：字段名是拼写正确的 accessCode，
// dateTime 用**秒**，并且登录请求同时带 loginDateTime、isContinue 为 false、regionId 为 8，
// 登出请求还要带 type。
//
// 【待验证】这套字段未经真实服务端验证——首次尝试时该出口 IP 已被限频封禁，
// 只留下「用 1.53 形状登录会拿到 HTTP 500」这一条观测。
var modernLoginShape = LoginShape{
	AccessCodeField:      "accessCode",
	TimestampSeconds:     true,
	IncludeLoginDateTime: true,
	ContinueOnLogin:      false,
	RegionID:             8,
	LogoutType:           5,
	ClientID:             DefaultClientID,
	PlaceID:              DefaultPlaceID,
}

// Versions 是全部已知协议参数表。新增版本只需在这里加一项。
var Versions = map[string]Version{
	"1.40": {
		Encoding:          "1.40",
		ObfuscateParam:    "BEs2D5vW",
		Key:               []byte("n7bx6:@Fg_:2;5E89Phy7AyIcpxEQ:R@"),
		IV:                []byte(";;KjR1C3hgB1ovXa"),
		ZlibBeforeEncrypt: false,
		Login:             legacyLoginShape,
	},
	"1.50": {
		Encoding:          "1.50",
		ObfuscateParam:    "B44df8yT",
		Key:               []byte("a>32bVP7v<63BVLkY[xM>daZ1s9MBP<R"),
		IV:                []byte("d6xHIKq]1J]Dt^ue"),
		ZlibBeforeEncrypt: false,
		Login:             legacyLoginShape,
	},
	"1.51": {
		Encoding:          "1.51",
		ObfuscateParam:    "B44df8yT",
		Key:               []byte("a>32bVP7v<63BVLkY[xM>daZ1s9MBP<R"),
		IV:                []byte("d6xHIKq]1J]Dt^ue"),
		ZlibBeforeEncrypt: false,
		Login:             legacyLoginShape,
	},
	"1.52": {
		Encoding:          "1.52",
		ObfuscateParam:    "B44df8yT",
		Key:               []byte("a>32bVP7v<63BVLkY[xM>daZ1s9MBP<R"),
		IV:                []byte("d6xHIKq]1J]Dt^ue"),
		ZlibBeforeEncrypt: false,
		Login:             legacyLoginShape,
	},
	"1.53": {
		Encoding:          "1.53",
		ObfuscateParam:    "LatuAa81",
		Key:               []byte("o2U8F6<adcYl25f_qwx_n]5_qxRcbLN>"),
		IV:                []byte("AL<G:k:X6Vu7@_U]"),
		ZlibBeforeEncrypt: true,
		Login:             legacyLoginShape,
	},
	"1.55": {
		Encoding:          "1.55",
		ObfuscateParam:    "8bF76dE9",
		Key:               []byte("FKM2JX:VjZNK6hc:A0<JU:i5oR7LA]9W"),
		IV:                []byte("F>;24DjU9W6ZsRH["),
		ZlibBeforeEncrypt: true,
		Login:             modernLoginShape,
	},
}

// DefaultVersion 是未显式指定 --version 时使用的版本。
//
// 取最新一版而不是文档里着力描述的 1.53：实测服务端对不再接受的协议版本会返回
// 「HTTP 200 + 0 字节」，与出口 IP 被阻断的症状完全一样。把默认值留在旧版本上，
// 使用者看到的就是一个假的「IP 被阻断」。
const DefaultVersion = "1.55"

// LookupVersion 按版本号取参数；未知版本返回全部可用版本，便于提示用户可选值。
func LookupVersion(name string) (Version, error) {
	if name == "" {
		name = DefaultVersion
	}
	v, ok := Versions[name]
	if !ok {
		return Version{}, fmt.Errorf("%w：%q，可用版本 %v", ErrUnsupportedVersion, name, SupportedVersions())
	}
	return v, nil
}

// SupportedVersions 返回参数表里全部版本号，顺序固定以便稳定输出。
func SupportedVersions() []string {
	return []string{"1.40", "1.50", "1.51", "1.52", "1.53", "1.55"}
}

// Validate 检查参数表本身是否自洽：AES 参数长度、必填字段。
func (v Version) Validate() error {
	if v.Encoding == "" {
		return fmt.Errorf("%w: 版本缺少 Encoding", ErrBadConfig)
	}
	if v.ObfuscateParam == "" {
		return fmt.Errorf("%w: 版本 %s 缺少 ObfuscateParam", ErrBadConfig, v.Encoding)
	}
	if len(v.Key) != 32 {
		return fmt.Errorf("%w: 版本 %s 的 AES Key 长度 %d，应为 32", ErrBadConfig, v.Encoding, len(v.Key))
	}
	if len(v.IV) != 16 {
		return fmt.Errorf("%w: 版本 %s 的 AES IV 长度 %d，应为 16", ErrBadConfig, v.Encoding, len(v.IV))
	}
	if v.Login.AccessCodeField == "" {
		return fmt.Errorf("%w: 版本 %s 缺少 Login.AccessCodeField", ErrBadConfig, v.Encoding)
	}
	return nil
}
