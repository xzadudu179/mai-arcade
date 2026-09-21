package protocol

import "github.com/xzadudu179/maimai-arcade/internal/model"

// achievementScale 是机台 achievement 字段的刻度：它是达成率 ×10000 的整数。
const achievementScale = 10000

// scoresFromMusic 把分页响应摊平成归一化成绩，并丢弃未游玩过的条目。
func scoresFromMusic(resp getUserMusicResponse) []model.Score {
	out := make([]model.Score, 0, len(resp.UserMusicList))
	for _, group := range resp.UserMusicList {
		for _, detail := range group.UserMusicDetailList {
			if detail.PlayCount <= 0 {
				continue
			}
			out = append(out, toScore(detail))
		}
	}
	return out
}

// toScore 把机台明细转成归一化成绩。
//
// Combo 与 Sync 不做范围收敛：越界值会在映射为查分器字符串时落成空串，
// 这样异常数据既不会污染上传内容，也不会被静默改写成看似合法的标记。
func toScore(detail userMusicDetail) model.Score {
	return model.Score{
		MusicID:     detail.MusicID,
		Level:       model.LevelIndex(detail.Level),
		Achievement: float64(detail.Achievement) / achievementScale,
		DXScore:     detail.DeluxscoreMax,
		Combo:       model.ComboStatus(detail.ComboStatus),
		Sync:        model.SyncStatus(detail.SyncStatus),
		PlayCount:   detail.PlayCount,
	}
}

// RatingItem 是官方 ratingList 中的一项。
type RatingItem struct {
	MusicID int              `json:"musicId"`
	Level   model.LevelIndex `json:"level"`
	Point   int              `json:"point"`
}

// Profile 是账号资料：头像、姓名框、牌子、称号与评级。
type Profile struct {
	UserID        int          `json:"userId"`
	UserName      string       `json:"userName"`
	Rating        int          `json:"rating"`
	HighestRating int          `json:"highestRating"`
	IconID        int          `json:"iconId"`
	NameplateID   int          `json:"nameplateId"`
	FrameID       int          `json:"frameId"`
	TrophyID      int          `json:"trophyId"`
	PartnerID     int          `json:"partnerId"`
	ClassID       int          `json:"classId"`
	RatingList    []RatingItem `json:"ratingList,omitempty"`
}

// mergeProfile 把两个接口的响应合成一份资料。
//
// 两个接口的用途不同：preview 给外观与当前评级，data 给历史最高评级与 ratingList，
// 任一接口缺失的字段由另一个补，缺字段不视为错误。
func mergeProfile(preview userPreviewResponse, data getUserDataResponse) Profile {
	rating := preview.Rating
	if rating == 0 {
		rating = data.Rating
	}
	userName := preview.UserName
	if userName == "" {
		userName = data.UserName
	}
	userID := preview.UserID
	if userID == 0 {
		userID = data.UserID
	}

	list := data.UserRatingList
	if len(list) == 0 {
		list = data.RatingList
	}
	items := make([]RatingItem, 0, len(list))
	for _, it := range list {
		items = append(items, RatingItem{
			MusicID: it.MusicID,
			Level:   model.LevelIndex(it.Level),
			Point:   it.Point,
		})
	}

	return Profile{
		UserID:        userID,
		UserName:      userName,
		Rating:        rating,
		HighestRating: data.HighestRating,
		IconID:        preview.IconID,
		NameplateID:   preview.NameplateID,
		FrameID:       preview.FrameID,
		TrophyID:      preview.TrophyID,
		PartnerID:     preview.PartnerID,
		ClassID:       preview.ClassID,
		RatingList:    items,
	}
}
