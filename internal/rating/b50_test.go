package rating

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// fixture 是 testdata 里的真值样本结构。
//
// 数据取自水鱼公开的 /api/maimaidxprober/player/test_data（无需鉴权的官方测试数据，
// 不含任何凭证）：其中每条成绩都带服务端算好的 ra，以及服务端给出的 rating 总和。
type fixture struct {
	Comment       string `json:"comment"`
	RatingVectors []struct {
		DS   float64 `json:"ds"`
		Ach  float64 `json:"ach"`
		RA   int     `json:"ra"`
		Note string  `json:"note"`
	} `json:"rating_vectors"`
	B50Case struct {
		ExpectedRating int `json:"expected_rating"`
		TopNew         int `json:"top_new"`
		TopOld         int `json:"top_old"`
		Records        []struct {
			SongID       int     `json:"song_id"`
			Type         string  `json:"type"`
			LevelIndex   int     `json:"level_index"`
			Achievements float64 `json:"achievements"`
			DS           float64 `json:"ds"`
			RA           int     `json:"ra"`
			IsNew        bool    `json:"is_new"`
		} `json:"records"`
		Songs []struct {
			ID    int       `json:"id"`
			Title string    `json:"title"`
			Type  string    `json:"type"`
			DS    []float64 `json:"ds"`
			IsNew bool      `json:"is_new"`
		} `json:"songs"`
	} `json:"b50_case"`
}

// loadFixture 读取真值样本。
func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/rating_vectors.json")
	if err != nil {
		t.Fatalf("读取 testdata 失败: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("解析 testdata 失败: %v", err)
	}
	if len(f.RatingVectors) == 0 || f.B50Case.ExpectedRating == 0 {
		t.Fatal("testdata 内容不完整")
	}
	return f
}

// TestSingleRatingMatchesServerValues 用真实数据断言 ra 公式。
//
// 每条向量都是「水鱼算出的 ra」，本实现必须逐条复现；这是 F12 从推测变成已验证的依据。
func TestSingleRatingMatchesServerValues(t *testing.T) {
	f := loadFixture(t)

	checked := 0
	utageSkipped := 0
	for _, v := range f.RatingVectors {
		if strings.Contains(v.Note, "utage") {
			// 宴谱在服务端 ra 被置 0，本项目在上游就把它排除掉，不参与公式比对。
			utageSkipped++
			continue
		}
		t.Run(strings.ReplaceAll(v.Note, "=", "_"), func(t *testing.T) {
			if got := SingleRating(v.DS, v.Ach); got != v.RA {
				t.Errorf("SingleRating(ds=%v, ach=%v) = %d, 服务端为 %d", v.DS, v.Ach, got, v.RA)
			}
		})
		checked++
	}

	if checked < 20 {
		t.Errorf("只比对了 %d 条向量，样本太少", checked)
	}
	if utageSkipped == 0 {
		t.Error("样本里应当含宴谱向量，用来固定「宴谱不计入」这个约定")
	}
}

// TestCalcReproducesServerRating 用真实数据断言 b50 结构与总和。
//
// 期望值是水鱼返回的 rating 总和：只要 rating 一致，就说明新曲 15 + 旧曲 35 的
// 分组方式、排序方式与 ra 求和方式全都对上了。
func TestCalcReproducesServerRating(t *testing.T) {
	f := loadFixture(t)

	songs := make([]chart.Song, 0, len(f.B50Case.Songs))
	for _, s := range f.B50Case.Songs {
		songs = append(songs, chart.Song{ID: s.ID, Title: s.Title, Type: s.Type, DS: s.DS, IsNew: s.IsNew})
	}
	index := chart.NewIndex(songs)

	scores := make([]model.Score, 0, len(f.B50Case.Records))
	wantRA := make(map[int]int, len(f.B50Case.Records))
	for _, r := range f.B50Case.Records {
		scores = append(scores, model.Score{
			MusicID:     r.SongID,
			Level:       model.LevelIndex(r.LevelIndex),
			Achievement: r.Achievements,
			PlayCount:   1,
		})
		wantRA[r.SongID] = r.RA
	}

	result := Calc(scores, index)

	if result.Rating != f.B50Case.ExpectedRating {
		t.Errorf("rating = %d, 服务端为 %d", result.Rating, f.B50Case.ExpectedRating)
	}
	if len(result.New) != f.B50Case.TopNew {
		t.Errorf("新曲条数 = %d, 期望 %d", len(result.New), f.B50Case.TopNew)
	}
	if len(result.Old) != f.B50Case.TopOld {
		t.Errorf("旧曲条数 = %d, 期望 %d", len(result.Old), f.B50Case.TopOld)
	}

	sum := 0
	for _, entry := range append(append([]Entry{}, result.New...), result.Old...) {
		sum += entry.RA
		want, ok := wantRA[entry.Score.MusicID]
		if !ok {
			t.Errorf("musicId=%d 不在样本里", entry.Score.MusicID)
			continue
		}
		if entry.RA != want {
			t.Errorf("musicId=%d 的 ra = %d, 服务端为 %d", entry.Score.MusicID, entry.RA, want)
		}
	}
	if sum != result.Rating {
		t.Errorf("逐条 ra 之和 %d 与 Rating 字段 %d 不一致", sum, result.Rating)
	}
}

// TestCoefficientTableBoundaries 断言系数表的档位边界，每档下沿都必须命中。
func TestCoefficientTableBoundaries(t *testing.T) {
	tests := []struct {
		name string
		ach  float64
		want float64
	}{
		{"SSS+ 下沿", 100.5, 22.4},
		{"SSS+ 超出上限仍按 SSS+ 档", 101.0, 22.4},
		{"SSS 区间上沿", 100.4999, 21.6},
		{"SSS 下沿", 100.0, 21.6},
		{"SS+ 区间上沿", 99.9999, 21.1},
		{"SS+ 下沿", 99.5, 21.1},
		{"SS 区间上沿", 99.4999, 20.8},
		{"SS 下沿", 99.0, 20.8},
		{"S+ 下沿", 98.0, 20.3},
		{"S 下沿", 97.0, 20.0},
		{"AAA 下沿", 94.0, 16.8},
		{"AA 下沿", 90.0, 15.2},
		{"A 下沿", 80.0, 13.6},
		{"BBB 下沿", 75.0, 12.0},
		{"BB 下沿", 70.0, 11.2},
		{"B 下沿", 60.0, 9.6},
		{"C 下沿", 50.0, 8.0},
		{"D 下沿", 40.0, 6.4},
		{"30% 档", 30.0, 4.8},
		{"20% 档", 20.0, 3.2},
		{"10% 档", 10.0, 1.6},
		{"0% 档", 0.0, 0.0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Coefficient(tc.ach); got != tc.want {
				t.Errorf("Coefficient(%v) = %v, 期望 %v", tc.ach, got, tc.want)
			}
		})
	}
}

// TestSingleRatingCapsAchievementAndRejectsDegenerateInput 断言达成率封顶与退化输入。
func TestSingleRatingCapsAchievementAndRejectsDegenerateInput(t *testing.T) {
	tests := []struct {
		name string
		ds   float64
		ach  float64
		want int
	}{
		{"定数为零", 0, 100.5, 0},
		{"定数为负", -1, 100.5, 0},
		{"达成率为零", 13.7, 0, 0},
		{"达成率为负", 13.7, -5, 0},
		{"100.5 与 101 结果相同（达成率封顶）", 13.7, 100.5, SingleRating(13.7, 101.0)},
		{"向下取整", 14.3, 100.6091, 321},
		{"恰好整除时不多算一分", 14.0, 100.5, 315},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SingleRating(tc.ds, tc.ach); got != tc.want {
				t.Errorf("SingleRating(%v, %v) = %d, 期望 %d", tc.ds, tc.ach, got, tc.want)
			}
		})
	}
}

// TestCalcSkipsUnrateableScores 断言无法参与官方计算的成绩被跳过。
func TestCalcSkipsUnrateableScores(t *testing.T) {
	index := chart.NewIndex([]chart.Song{
		{ID: 1001, Title: "普通曲", Type: "DX", DS: []float64{3, 7, 10, 13}, IsNew: false},
		{ID: 1002, Title: "新曲", Type: "DX", DS: []float64{4, 8, 11, 14}, IsNew: true},
		// 定数为 0 表示索引里没有可用定数。
		{ID: 1003, Title: "无定数曲", Type: "DX", DS: []float64{}, IsNew: false},
	})

	scores := []model.Score{
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		{MusicID: 1002, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		// 宴谱：不参与 b50。
		{MusicID: 100508, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		// 索引里没有这首曲目。
		{MusicID: 9999, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		// 该难度在定数数组里不存在。
		{MusicID: 1001, Level: model.LevelReMaster, Achievement: 100.5, PlayCount: 1},
	}

	result := Calc(scores, index)

	if len(result.New) != 1 {
		t.Errorf("新曲条数 = %d, 期望 1（只有 1002 可算）", len(result.New))
	}
	if len(result.Old) != 1 {
		t.Errorf("旧曲条数 = %d, 期望 1（只有 1001 可算）", len(result.Old))
	}
	if result.Rating != SingleRating(14, 100.5)+SingleRating(13, 100.5) {
		t.Errorf("rating = %d, 与逐条计算之和不符", result.Rating)
	}
}

// TestCalcTruncatesWhenNotEnoughScores 断言成绩不足时按实际条数截断，而不是补零。
func TestCalcTruncatesWhenNotEnoughScores(t *testing.T) {
	songs := []chart.Song{{ID: 1001, Title: "A", Type: "DX", DS: []float64{3, 7, 10, 13}}}
	index := chart.NewIndex(songs)

	scores := []model.Score{
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		{MusicID: 1001, Level: model.LevelExpert, Achievement: 100.5, PlayCount: 1},
	}

	result := Calc(scores, index)

	if len(result.Old) != 2 {
		t.Errorf("旧曲条数 = %d, 期望 2", len(result.Old))
	}
	if len(result.New) != 0 {
		t.Errorf("新曲条数 = %d, 期望 0", len(result.New))
	}
	if result.Rating != SingleRating(13, 100.5)+SingleRating(10, 100.5) {
		t.Errorf("rating = %d, 期望两条之和", result.Rating)
	}
}

// TestCalcSortsByRADescending 断言结果按 ra 降序，且 ra 相同时顺序稳定。
func TestCalcSortsByRADescending(t *testing.T) {
	songs := make([]chart.Song, 0, 5)
	scores := make([]model.Score, 0, 5)
	for i := 0; i < 5; i++ {
		id := 2000 + i
		ds := 10.0 + float64(i)
		songs = append(songs, chart.Song{ID: id, Title: "T", Type: "DX", DS: []float64{ds}})
		scores = append(scores, model.Score{MusicID: id, Level: model.LevelBasic, Achievement: 100.5, PlayCount: 1})
	}
	index := chart.NewIndex(songs)

	result := Calc(scores, index)

	for i := 1; i < len(result.Old); i++ {
		if result.Old[i-1].RA < result.Old[i].RA {
			t.Fatalf("结果未按 ra 降序：第 %d 条 %d < 第 %d 条 %d",
				i-1, result.Old[i-1].RA, i, result.Old[i].RA)
		}
	}
	if result.Old[0].DS != 14.0 {
		t.Errorf("首条定数 = %v, 期望最高的 14.0", result.Old[0].DS)
	}
}
