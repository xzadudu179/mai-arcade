// Package rating 由机台成绩计算单曲 ra 与 b50。
//
// 单曲 ra = floor(定数 × 评级系数 × min(达成率, 100.5) / 100)，系数表见 coefficientTable。
// 总 rating = 新曲 top15 + 旧曲 top35 的 ra 之和（宴谱不计入）。
//
// 这两条结论都由 testdata/rating_vectors.json 里的真实数据断言：该文件取自水鱼公开的
// player/test_data，其中每条成绩都带服务端算好的 ra，以及服务端给出的 rating 总和。
package rating

import (
	"sort"

	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// achievementCap 是计入 rating 的达成率上限：超过部分不参与计算。
const achievementCap = 100.5

// coefficientSegment 是系数表的一段：达成率达到 MinAchievement 时使用 Coefficient。
type coefficientSegment struct {
	MinAchievement float64
	Coefficient    float64
}

// coefficientTable 是 Gen 3（Splash PLUS 至今）的评级系数表，按达成率降序排列。
var coefficientTable = []coefficientSegment{
	{100.5, 22.4}, // SSS+
	{100.0, 21.6}, // SSS
	{99.5, 21.1},  // SS+
	{99.0, 20.8},  // SS
	{98.0, 20.3},  // S+
	{97.0, 20.0},  // S
	{94.0, 16.8},  // AAA
	{90.0, 15.2},  // AA
	{80.0, 13.6},  // A
	{75.0, 12.0},  // BBB
	{70.0, 11.2},  // BB
	{60.0, 9.6},   // B
	{50.0, 8.0},   // C
	{40.0, 6.4},   // D
	{30.0, 4.8},
	{20.0, 3.2},
	{10.0, 1.6},
	{0.0, 0.0},
}

// Coefficient 返回某达成率对应的评级系数；低于最低档返回 0。
func Coefficient(achievement float64) float64 {
	for _, seg := range coefficientTable {
		if achievement >= seg.MinAchievement {
			return seg.Coefficient
		}
	}
	return 0
}

// SingleRating 计算单谱面 ra；达成率为 0 或定数非正时返回 0。
func SingleRating(ds, achievement float64) int {
	if ds <= 0 || achievement <= 0 {
		return 0
	}
	capped := achievement
	if capped > achievementCap {
		capped = achievementCap
	}
	return int(ds * Coefficient(achievement) * capped / 100)
}

// TopNew 与 TopOld 是 b50 的两组配额。
const (
	TopNew = 15
	TopOld = 35
)

// Entry 是 b50 中的一条：成绩本身加上算出的 ra 与定数。
type Entry struct {
	Score model.Score
	DS    float64
	RA    int
	Title string
}

// B50 是最终结果：两组条目加上 ra 总和。
type B50 struct {
	New    []Entry
	Old    []Entry
	Rating int
}

// SongSource 提供 b50 所需的曲目信息；chart.Index 直接满足该接口。
type SongSource interface {
	// DS 返回某谱面的定数。
	DS(musicID int, level model.LevelIndex) (float64, bool)
	// IsNew 报告曲目是否属于当前版本新曲。
	IsNew(musicID int) bool
	// Title 返回曲名，仅用于展示。
	Title(musicID int) (string, bool)
}

// Calc 按新曲/旧曲分组取 ra 最高的若干条并求和。
//
// 定数缺失、宴谱、定数非正的条目一律跳过：它们无法参与官方 rating 计算，
// 硬算出来的数值只会让总 rating 偏离机台显示值。
func Calc(scores []model.Score, songs SongSource) B50 {
	var newEntries, oldEntries []Entry
	for _, s := range scores {
		if s.IsUtage() {
			continue
		}
		ds, ok := songs.DS(s.MusicID, s.Level)
		if !ok || ds <= 0 {
			continue
		}
		entry := Entry{Score: s, DS: ds, RA: SingleRating(ds, s.Achievement)}
		if title, ok := songs.Title(s.MusicID); ok {
			entry.Title = title
		}
		if songs.IsNew(s.MusicID) {
			newEntries = append(newEntries, entry)
		} else {
			oldEntries = append(oldEntries, entry)
		}
	}

	sortByRADesc(newEntries)
	sortByRADesc(oldEntries)

	result := B50{
		New: truncate(newEntries, TopNew),
		Old: truncate(oldEntries, TopOld),
	}
	for _, e := range result.New {
		result.Rating += e.RA
	}
	for _, e := range result.Old {
		result.Rating += e.RA
	}
	return result
}

// sortByRADesc 按 ra 降序排序；ra 相同时按 musicId 升序，保证结果稳定可复现。
func sortByRADesc(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].RA != entries[j].RA {
			return entries[i].RA > entries[j].RA
		}
		return entries[i].Score.MusicID < entries[j].Score.MusicID
	})
}

func truncate(entries []Entry, n int) []Entry {
	if len(entries) <= n {
		return entries
	}
	return entries[:n]
}
