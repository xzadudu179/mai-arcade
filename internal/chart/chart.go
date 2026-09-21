// Package chart 维护 musicId 到曲目元数据的索引：歌名、SD/DX 类型、各难度定数与新旧曲标记。
//
// 它是 protocol 与 sync 之间的桥：机台成绩只给 musicId，而水鱼以「歌名 + 类型 + 难度」定位曲目，
// 所以同步必须先经这里把 musicId 翻成歌名；b50 也算定数。
package chart

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// Song 是一首曲目的元数据。
type Song struct {
	// ID 是官方 musicId：<10000 为 SD 谱，>=10000 为 DX 谱，>=100000 为宴谱。
	ID int

	// Title 是水鱼侧的曲名。水鱼以上传内容里的歌名精确匹配曲目，因此必须用这个值。
	Title string

	// Type 是 "SD" 或 "DX"。
	Type string

	// DS 是按 level_index 排列的定数；不同谱面难度数量不同，可能少于 5 项。
	DS []float64

	// IsNew 标记是否为当前版本新曲，b50 的新曲 15 由它划分。
	IsNew bool
}

// Source 提供按 musicId 反查曲目的能力。
type Source interface {
	// Song 返回 musicId 对应的曲目；未知曲目返回 false。
	Song(musicID int) (Song, bool)
}

// Index 是曲目索引，并发只读安全。
type Index struct {
	byID map[int]Song
}

// NewIndex 用曲目列表构造索引；id 重复时后者覆盖前者。
func NewIndex(songs []Song) *Index {
	byID := make(map[int]Song, len(songs))
	for _, s := range songs {
		if s.ID <= 0 || s.Title == "" || s.Type == "" {
			continue
		}
		byID[s.ID] = s
	}
	return &Index{byID: byID}
}

// Song 返回 musicId 对应的曲目。
func (i *Index) Song(musicID int) (Song, bool) {
	if i == nil {
		return Song{}, false
	}
	s, ok := i.byID[musicID]
	return s, ok
}

// Usable 报告这条曲目信息是否足以用于上传成绩。
//
// 查分器靠歌名匹配曲目，缺曲名或缺类型都写不出去；索引里个别条目只有 id，
// 所有「能不能映射」的判断都应当以它为准。
func (s Song) Usable() bool { return s.Title != "" && s.Type != "" }

// Len 返回索引中的曲目数。
func (i *Index) Len() int {
	if i == nil {
		return 0
	}
	return len(i.byID)
}

// DS 返回某谱面的定数；曲目未知或该难度不存在时返回 false。
func (i *Index) DS(musicID int, level model.LevelIndex) (float64, bool) {
	s, ok := i.Song(musicID)
	if !ok {
		return 0, false
	}
	if int(level) >= len(s.DS) {
		return 0, false
	}
	return s.DS[level], true
}

// IsNew 报告曲目是否属于当前版本新曲；未知曲目返回 false。
func (i *Index) IsNew(musicID int) bool {
	s, ok := i.Song(musicID)
	return ok && s.IsNew
}

// Title 返回曲名；未知曲目返回 false。
func (i *Index) Title(musicID int) (string, bool) {
	s, ok := i.Song(musicID)
	if !ok {
		return "", false
	}
	return s.Title, true
}

// rawSong 是水鱼 music_data 的原始条目。
type rawSong struct {
	ID        flexInt   `json:"id"`
	Title     string    `json:"title"`
	Type      string    `json:"type"`
	DS        []float64 `json:"ds"`
	BasicInfo struct {
		IsNew bool `json:"is_new"`
	} `json:"basic_info"`
}

// flexInt 解析水鱼返回的 id：官方是字符串，但不同部署可能给数字。
type flexInt int

// UnmarshalJSON 同时接受 JSON 数字与字符串形式。
func (f *flexInt) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("id %q 不是整数: %w", s, err)
		}
		*f = flexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// SongsFromMusicData 把 music_data 的响应体解析成曲目列表。
func SongsFromMusicData(body []byte) ([]Song, error) {
	var raw []rawSong
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析曲目数据失败（%d 字节）: %w", len(body), err)
	}
	songs := make([]Song, 0, len(raw))
	for _, r := range raw {
		songs = append(songs, Song{
			ID:    int(r.ID),
			Title: r.Title,
			Type:  r.Type,
			DS:    r.DS,
			IsNew: r.BasicInfo.IsNew,
		})
	}
	return songs, nil
}
