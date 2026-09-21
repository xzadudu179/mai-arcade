package main

import (
	"context"

	"github.com/xzadudu179/maimai-arcade/internal/rating"
)

// b50Output 是 b50 的输出结构。
type b50Output struct {
	UserID       int        `json:"userId"`
	Version      string     `json:"version"`
	Rating       int        `json:"rating"`
	NewCount     int        `json:"newCount"`
	OldCount     int        `json:"oldCount"`
	UnratedCount int        `json:"unratedCount"`
	New          []b50Entry `json:"new"`
	Old          []b50Entry `json:"old"`
	Note         string     `json:"note"`
}

// b50Entry 是 b50 中的一条。
type b50Entry struct {
	MusicID     int     `json:"musicId"`
	Title       string  `json:"title"`
	Level       string  `json:"level"`
	DS          float64 `json:"ds"`
	Achievement float64 `json:"achievement"`
	RA          int     `json:"ra"`
	Combo       string  `json:"combo"`
	Sync        string  `json:"sync"`
}

// runB50 读 stdin 的二维码，拉取成绩并计算 b50。
//
// 定数来自查分器的全曲库，新旧曲划分同样来自它：机台成绩本身不带定数。
// 定数缺失的曲目会被跳过，unratedCount 就是被跳过的条数。
func runB50(ctx context.Context, e *environment, args []string) error {
	fs := e.newFlagSet("b50")
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

	session, err := svc.Fetch(ctx, sgid)
	if err != nil {
		return err
	}
	index, err := svc.ChartIndex(ctx)
	if err != nil {
		return err
	}

	result := rating.Calc(session.Scores, index)

	unrated := 0
	for _, score := range session.Scores {
		if score.IsUtage() {
			continue
		}
		if _, ok := index.DS(score.MusicID, score.Level); !ok {
			unrated++
		}
	}

	return emitData(e, "b50", b50Output{
		UserID:       session.UserID,
		Version:      svc.Version(),
		Rating:       result.Rating,
		NewCount:     len(result.New),
		OldCount:     len(result.Old),
		UnratedCount: unrated,
		New:          toB50Entries(result.New),
		Old:          toB50Entries(result.Old),
		Note:         "rating = 新曲 ra 最高的 15 条 + 旧曲 ra 最高的 35 条之和；ra = floor(定数 × 系数 × min(达成率,100.5)/100)；宴谱不计入",
	})
}

// toB50Entries 把计算结果转成输出结构。
func toB50Entries(entries []rating.Entry) []b50Entry {
	out := make([]b50Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, b50Entry{
			MusicID:     entry.Score.MusicID,
			Title:       entry.Title,
			Level:       entry.Score.Level.String(),
			DS:          entry.DS,
			Achievement: entry.Score.Achievement,
			RA:          entry.RA,
			Combo:       entry.Score.Combo.DivingFish(),
			Sync:        entry.Score.Sync.DivingFish(),
		})
	}
	return out
}
