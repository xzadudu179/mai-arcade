package protocol

import (
	"encoding/json"
	"time"
)

// 机台 API 变体名。必须与官方名逐字符一致，否则 api_hash 算不对、请求会被当作未知路径。
const (
	APIPing           = "Ping"
	APIUserLogin      = "UserLoginApi"
	APIGetUserMusic   = "GetUserMusicApi"
	APIUserLogout     = "UserLogoutApi"
	APIGetUserData    = "GetUserDataApi"
	APIGetUserPreview = "GetUserPreviewApi"
)

// AimeDB 换账号接口的固定参数。
const (
	AimeDBURL = "http://ai.sys-allnet.cn/wc_aime/api/get_data"

	AimeUserAgent  = "WC_AIME_LIB"
	AimeOpenGameID = "MAID"

	// AimeChipID 是 1.53 起使用的机台标识。
	AimeChipID = "A63E-01C28055905"

	// AimeChipIDLegacy 是 1.53 之前的机台标识。
	AimeChipIDLegacy = "A63E-01E68606624"

	// AimeCommonKey 参与 key 的 SHA-256 计算。
	AimeCommonKey = "XcW5FW4cPArBXEk4vzKz3CIrMuA5EVVW"
)

// 登录/登出请求需要的机台与门店标识。
//
// 两者都不可为空：服务端在建立会话时要拿它们做匹配与记录，传空实测得到 HTTP 500
// （Tomcat 错误页，不是业务返回码），排查时极难定位到这两个字段。
const (
	// DefaultClientID 是 AimeChipID 去掉横线后的前 11 位，与 chipID 同源。
	DefaultClientID = "A63E01C2805"

	// DefaultPlaceID 是门店编号，取社区实现的通用默认值。
	DefaultPlaceID = "1403"
)

// DefaultTitleBaseURL 是标题服务器根地址；完整 URL 是它直接拼接 api_hash，没有额外路径段。
const DefaultTitleBaseURL = "https://maimai-gm.wahlap.com:42081/Maimai2Servlet/"

// musicPageSize 是拉成绩时的分页大小。机台上限未知，取大值以减少轮次。
const musicPageSize = 2000

// DefaultRequestInterval 是两次机台请求之间必须留出的最小间隔。
//
// 取值来自 GetGameSettingApi 的 gameSetting.requestInterval（实测 1200 毫秒）。
// 连发请求会被服务端丢弃并返回空体，症状与「会话无效」一模一样，
// 排查时极易误判——所以这里按协议要求主动节流，而不是靠调用方自觉。
const DefaultRequestInterval = 1200 * time.Millisecond

// aimeErrorReasons 是 AimeDB errorID 到可读原因与补救提示的映射。
var aimeErrorReasons = map[int]struct{ Reason, Hint string }{
	0:  {Reason: "成功"},
	1:  {Reason: "二维码过期（30 分钟档）", Hint: "重新从公众号获取二维码"},
	2:  {Reason: "二维码过期（10 分钟档）", Hint: "重新从公众号获取二维码"},
	50: {Reason: "签名错误", Hint: "检查 chipID 与时间戳时区（本地时区，Rust 版显式 UTC+8）"},
}

// titleReturnCodeReasons 是标题服务器 returnCode 到可读原因的映射。
//
// 表驱动而非 switch：新增码只改这里一行。
var titleReturnCodeReasons = map[int]struct{ Reason, Hint string }{
	1:   {Reason: "成功"},
	100: {Reason: "账号已在登录状态", Hint: "等会话超时（约 15 分钟）后重试，不要反复重试"},
	102: {Reason: "二维码过期", Hint: "重新从公众号获取二维码"},
	110: {Reason: "KeyChip 不匹配", Hint: "机台参数问题，检查 chipID 与协议版本"},
}

// aimeRequest 是 AimeDB 请求体，字段顺序与官方一致，序列化为紧凑 JSON。
type aimeRequest struct {
	ChipID     string `json:"chipID"`
	OpenGameID string `json:"openGameID"`
	Key        string `json:"key"`
	QRCode     string `json:"qrCode"`
	Timestamp  string `json:"timestamp"`
}

// aimeResponse 是 AimeDB 响应体。
//
// 实测响应比文档多两个字段：key 与 timestamp（服务端签名所用的那组值）。
// 文档 §2.3 只列了 errorID / userID / token，这里按实测补齐，避免把服务端给的信息静默丢掉。
type aimeResponse struct {
	ErrorID   int    `json:"errorID"`
	UserID    int    `json:"userID"`
	Token     string `json:"token"`
	Key       string `json:"key"`
	Timestamp string `json:"timestamp"`
}

// userLoginRequest 是 UserLoginApi 请求体。
//
// 字段名与时间单位随版本变化（见 Version.Login），因此按形状动态构造，
// 而不是写死一组 json tag：`acsessCode` 与 `accessCode` 都真实存在过，
// 抄错的那一份会被服务端判为缺字段并直接报 500。
type userLoginRequest struct {
	UserID        int
	RegionID      int
	DateTime      int64
	LoginDateTime int64
	AcsessCode    string
	PlaceID       string
	ClientID      string
	Token         string
	IsContinue    bool
	GenericFlag   int

	shape LoginShape
}

// MarshalJSON 按版本形状输出字段名。
func (r userLoginRequest) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"userId":      r.UserID,
		"regionId":    r.RegionID,
		"dateTime":    r.DateTime,
		"placeId":     r.PlaceID,
		"clientId":    r.ClientID,
		"token":       r.Token,
		"isContinue":  r.IsContinue,
		"genericFlag": r.GenericFlag,
	}
	out[r.shape.AccessCodeField] = r.AcsessCode
	if r.shape.IncludeLoginDateTime {
		out["loginDateTime"] = r.LoginDateTime
	}
	return json.Marshal(out)
}

// userLoginResponse 是 UserLoginApi 响应体。
type userLoginResponse struct {
	ReturnCode int `json:"returnCode"`
}

// userLogoutRequest 是 UserLogoutApi 请求体。
//
// 与登录一样按版本形状构造：新版本还要带 regionId / placeId / clientId / accessCode / type。
type userLogoutRequest struct {
	UserID        int
	LoginDateTime int64
	RegionID      int
	PlaceID       string
	ClientID      string
	AcsessCode    string

	shape LoginShape
}

// MarshalJSON 按版本形状输出字段名。
func (r userLogoutRequest) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"userId":        r.UserID,
		"loginDateTime": r.LoginDateTime,
	}
	if r.shape.IncludeLoginDateTime {
		out["regionId"] = r.RegionID
		out["placeId"] = r.PlaceID
		out["clientId"] = r.ClientID
		out[r.shape.AccessCodeField] = r.AcsessCode
		if r.shape.LogoutType != 0 {
			out["type"] = r.shape.LogoutType
		}
	}
	return json.Marshal(out)
}

// userLogoutResponse 是 UserLogoutApi 响应体。
type userLogoutResponse struct {
	ReturnCode int `json:"returnCode"`
}

// getUserMusicRequest 是 GetUserMusicApi 请求体。
type getUserMusicRequest struct {
	UserID    int `json:"userId"`
	NextIndex int `json:"nextIndex"`
	MaxCount  int `json:"maxCount"`
}

// userMusicDetail 是单条成绩明细。字段名照抄官方，含小写 s 的 DeluxscoreMax。
type userMusicDetail struct {
	MusicID       int `json:"musicId"`
	Level         int `json:"level"`
	PlayCount     int `json:"playCount"`
	Achievement   int `json:"achievement"`
	ComboStatus   int `json:"comboStatus"`
	SyncStatus    int `json:"syncStatus"`
	DeluxscoreMax int `json:"deluxscoreMax"`
	ScoreRank     int `json:"scoreRank"`
	ExtNum1       int `json:"extNum1"`
	ExtNum2       int `json:"extNum2"`
}

// userMusic 是分页中的一层容器，真实成绩在同名的明细列表里。
type userMusic struct {
	UserMusicDetailList []userMusicDetail `json:"userMusicDetailList"`
}

// getUserMusicResponse 是 GetUserMusicApi 响应体。
type getUserMusicResponse struct {
	NextIndex     int         `json:"nextIndex"`
	UserMusicList []userMusic `json:"userMusicList"`
}

// getUserDataRequest 是 GetUserDataApi 请求体。
type getUserDataRequest struct {
	UserID int `json:"userId"`
}

// ratingItem 是官方 ratingList 中的一项：曲目 + 难度 + 该谱面的 ra 点数。
type ratingItem struct {
	MusicID int `json:"musicId"`
	Level   int `json:"level"`
	Point   int `json:"point"`
}

// getUserDataResponse 是 GetUserDataApi 响应体。
//
// 【待验证】首次实现时无法在可用网络下取到真实响应校对字段名，解析保持宽松：
// 缺字段取零值，不报错。
type getUserDataResponse struct {
	UserID         int          `json:"userId"`
	UserName       string       `json:"userName"`
	Rating         int          `json:"rating"`
	HighestRating  int          `json:"highestRating"`
	RatingList     []ratingItem `json:"ratingList"`
	UserRatingList []ratingItem `json:"userRatingList"`
}

// userPreviewRequest 是 GetUserPreviewApi 请求体。
type userPreviewRequest struct {
	UserID        int    `json:"userId"`
	SegaIDAuthKey string `json:"segaIdAuthKey"`
	Token         string `json:"token"`
	ClientID      string `json:"clientId"`
}

// userPreviewResponse 是 GetUserPreviewApi 响应体，承载头像/姓名框/牌子/称号。
//
// 【待验证】同 getUserDataResponse，字段名未经真实响应校对，解析宽松。
type userPreviewResponse struct {
	UserID       int    `json:"userId"`
	UserName     string `json:"userName"`
	Rating       int    `json:"rating"`
	NameplateID  int    `json:"nameplateId"`
	FrameID      int    `json:"frameId"`
	IconID       int    `json:"iconId"`
	TrophyID     int    `json:"trophyId"`
	PartnerID    int    `json:"partnerId"`
	ClassID      int    `json:"classId"`
	LastPlayDate string `json:"lastPlayDate"`
}
