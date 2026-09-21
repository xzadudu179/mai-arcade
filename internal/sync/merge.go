package sync

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// divingFishName 是水鱼查分器的注册名。
const divingFishName = "divingfish"

// divingFishCredentialHeader 是水鱼读取成绩与上传成绩共用的鉴权头。
const divingFishCredentialHeader = "Import-Token"

// remoteRecord 是水鱼 GET /player/records 返回的一条成绩。
//
// 这是合并的输入：它带着服务器当前保存的 fc/fs，机台拿不到的标记必须从这里保留下来。
type remoteRecord struct {
	SongID       int     `json:"song_id"`
	Title        string  `json:"title"`
	Type         string  `json:"type"`
	LevelIndex   int     `json:"level_index"`
	Achievements float64 `json:"achievements"`
	DXScore      int     `json:"dxScore"`
	FC           string  `json:"fc"`
	FS           string  `json:"fs"`
}

// uploadRecord 是水鱼 POST /player/update_records 的单条请求。
//
// 字段名与取值都受服务端校验约束：fc 不在 fc/fcp/ap/app 内会被置空，
// fs 不在 sync/fs/fsp/fsd/fsdp 内会被置空，title 必须精确匹配歌名，level_index 必须是真实存在的难度。
type uploadRecord struct {
	Achievements float64 `json:"achievements"`
	DXScore      int     `json:"dxScore"`
	FC           string  `json:"fc"`
	FS           string  `json:"fs"`
	LevelIndex   int     `json:"level_index"`
	Title        string  `json:"title"`
	Type         string  `json:"type"`
}

// achievementDecimals 是回传达成率时保留的小数位：水鱼按 4 位小数理解这个字段。
const achievementDecimals = 4

// recordsBody 是 GET /player/records 的响应；服务端也可能直接返回数组，两种都接受。
type recordsBody struct {
	Records []remoteRecord `json:"records"`
}

// mergeResult 是一次合并的产物与统计。
type mergeResult struct {
	Records []uploadRecord

	// SkippedUnknown 是曲目索引里查不到、被跳过的成绩条数。
	SkippedUnknown int

	// SkippedInvalidLevel 是 level_index 越界、被跳过的条数。
	SkippedInvalidLevel int

	// SkipUtage 是宴谱条数，按规则不计入同步。
	SkipUtage int

	// PreservedMarks 是被保留下来的 FC/FS 标记条数（机台没给、服务器原有）。
	PreservedMarks int

	// RemoteOnly 是服务器有、机台未上报的曲目数，仅用于提示，不会被删除。
	RemoteOnly int
}

// merge 把机台成绩与服务器现状合并成待上传列表。
//
// 合并的核心约束：fc/fs 传空会清空服务器已有标记，而机台数据在部分情况下拿不到这两项，
// 因此机台缺的标记必须保留服务器原值；其余字段取两边的较大值，保证任何情况下都不会让已有成绩回退。
func merge(remote []remoteRecord, scores []model.Score, songs chart.Source) mergeResult {
	type key struct {
		title string
		kind  string
		level model.LevelIndex
	}

	current := make(map[key]remoteRecord, len(remote))
	for _, r := range remote {
		current[key{r.Title, r.Type, model.LevelIndex(r.LevelIndex)}] = r
	}

	result := mergeResult{}
	seen := make(map[key]struct{}, len(scores))
	ordered := make([]key, 0, len(scores))

	for _, score := range scores {
		if score.IsUtage() {
			result.SkipUtage++
			continue
		}
		if !score.Level.Valid() {
			result.SkippedInvalidLevel++
			continue
		}
		song, ok := songs.Song(score.MusicID)
		if !ok || song.Title == "" || song.Type == "" {
			result.SkippedUnknown++
			continue
		}

		k := key{song.Title, song.Type, score.Level}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		ordered = append(ordered, k)

		record := uploadRecord{
			Achievements: roundAchievement(score.Achievement),
			DXScore:      score.DXScore,
			FC:           score.Combo.DivingFish(),
			FS:           score.Sync.DivingFish(),
			LevelIndex:   int(score.Level),
			Title:        song.Title,
			Type:         song.Type,
		}

		if existing, found := current[k]; found {
			if record.FC == "" && existing.FC != "" {
				record.FC = existing.FC
				result.PreservedMarks++
			}
			if record.FS == "" && existing.FS != "" {
				record.FS = existing.FS
				result.PreservedMarks++
			}
			record.Achievements = math.Max(record.Achievements, roundAchievement(existing.Achievements))
			record.DXScore = maxInt(record.DXScore, existing.DXScore)
		}
		result.Records = append(result.Records, record)
	}

	for k := range current {
		if _, reported := seen[k]; !reported {
			result.RemoteOnly++
		}
	}
	return result
}

// roundAchievement 把达成率截到 4 位小数，避免浮点误差写出 100.99999999 这类值。
func roundAchievement(v float64) float64 {
	scale := math.Pow10(achievementDecimals)
	return math.Round(v*scale) / scale
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// parseRecordsBody 解析 GET /player/records 的响应，兼容「对象包 records」与「裸数组」两种形状。
//
// 空体与 null 一律报错而不是当作「没有成绩」：静默当成空会让紧随其后的上传
// 把服务器上已有的 FC/FS 全部清掉，这是最坏的失败方式——宁可失败也不要静默清空。
func parseRecordsBody(body []byte) ([]remoteRecord, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return nil, fmt.Errorf("响应体为空或为 null（%d 字节）", len(body))
	}

	if strings.HasPrefix(trimmed, "[") {
		var list []remoteRecord
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var wrapped recordsBody
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Records, nil
}
