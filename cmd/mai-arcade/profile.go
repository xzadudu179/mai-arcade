package main

import (
	"context"
)

// profileOutput 是 profile 的输出结构：只有外观编号与评级，没有凭证。
type profileOutput struct {
	UserID        int              `json:"userId"`
	UserName      string           `json:"userName"`
	Rating        int              `json:"rating"`
	HighestRating int              `json:"highestRating"`
	IconID        int              `json:"iconId"`
	NameplateID   int              `json:"nameplateId"`
	FrameID       int              `json:"frameId"`
	TrophyID      int              `json:"trophyId"`
	PartnerID     int              `json:"partnerId"`
	ClassID       int              `json:"classId"`
	TopRatings    []ratingItemJSON `json:"topRatings,omitempty"`
	Note          string           `json:"note"`
}

// ratingItemJSON 是官方 ratingList 的一项。
type ratingItemJSON struct {
	MusicID int `json:"musicId"`
	Level   int `json:"level"`
	Point   int `json:"point"`
}

// runProfile 读 stdin 的二维码，打印账号资料。
//
// 字段名尚未在可用网络下与真实响应校对过（见 README 的「已知限制」），
// 因此解析是宽松的：缺字段取零值，不会让整条命令失败。
func runProfile(ctx context.Context, e *environment, args []string) error {
	fs := e.newFlagSet("profile")
	if err := e.parseFlags(fs, args); err != nil {
		return err
	}
	logger, err := e.logger()
	if err != nil {
		return err
	}
	sgid, err := e.readSGID()
	if err != nil {
		return err
	}
	svc, err := e.prepare(ctx, logger)
	if err != nil {
		return err
	}

	profile, err := svc.Profile(ctx, sgid)
	if err != nil {
		return err
	}

	items := make([]ratingItemJSON, 0, len(profile.RatingList))
	for _, item := range profile.RatingList {
		items = append(items, ratingItemJSON{MusicID: item.MusicID, Level: int(item.Level), Point: item.Point})
	}

	return emitData(e, "profile", profileOutput{
		UserID:        profile.UserID,
		UserName:      profile.UserName,
		Rating:        profile.Rating,
		HighestRating: profile.HighestRating,
		IconID:        profile.IconID,
		NameplateID:   profile.NameplateID,
		FrameID:       profile.FrameID,
		TrophyID:      profile.TrophyID,
		PartnerID:     profile.PartnerID,
		ClassID:       profile.ClassID,
		TopRatings:    items,
		Note:          "领取字段名未经真实响应校对（GetUserDataApi / GetUserPreviewApi），缺字段会取零值",
	})
}
