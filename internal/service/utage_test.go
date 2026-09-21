package service

import (
	"testing"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// utageTestIndex 构造含宴谱的曲目索引。
func utageTestIndex() *chart.Index {
	return chart.NewIndex([]chart.Song{
		{ID: 1001, Title: "普通曲", Type: "DX", DS: []float64{3, 7, 10, 13}},
		{ID: 100508, Title: "[協]恋愛裁判", Type: "DX", DS: []float64{13.0}},
	})
}

// TestUtageDetail 断言宴谱明细的折算、排序、未命中计数与截断。
//
// 明细是用户在上传前判断宴谱能不能同步的唯一依据，折算错了（比如照抄 level=5）
// 会让人以为查分器那边也会照收。
func TestUtageDetail(t *testing.T) {
	scores := []model.Score{
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.0},
		{MusicID: 100508, Level: model.LevelUtage, Achievement: 99.5, DXScore: 2100,
			Combo: model.ComboFC, Sync: model.SyncFS},
		{MusicID: 100999, Level: model.LevelUtage, Achievement: 98.0},
		{MusicID: 100022, Level: model.LevelUtage, Achievement: 97.0},
	}

	samples, unmapped := utageDetail(scores, utageTestIndex(), 0)

	if len(samples) != 3 {
		t.Fatalf("明细条数 = %d, 期望 3（只含宴谱）", len(samples))
	}
	if unmapped != 2 {
		t.Errorf("未命中数 = %d, 期望 2（100999 与 100022）", unmapped)
	}
	// 按 musicId 升序：100022 → 100508 → 100999
	if samples[0].MusicID != 100022 || samples[2].MusicID != 100999 {
		t.Errorf("明细未按 musicId 排序：%d, %d, %d",
			samples[0].MusicID, samples[1].MusicID, samples[2].MusicID)
	}

	hit := samples[1]
	if hit.Title != "[協]恋愛裁判" || hit.Type != "DX" {
		t.Errorf("曲名/类型 = %q/%q, 期望从索引解析出来", hit.Title, hit.Type)
	}
	if hit.LevelIndex != int(model.UtageLevelIndex) || hit.RawLevel != int(model.LevelUtage) {
		t.Errorf("levelIndex/rawLevel = %d/%d, 期望 %d/%d",
			hit.LevelIndex, hit.RawLevel, model.UtageLevelIndex, model.LevelUtage)
	}
	if hit.Combo != "fc" || hit.Sync != "fs" {
		t.Errorf("combo/sync = %q/%q, 期望 fc/fs", hit.Combo, hit.Sync)
	}
	if samples[0].Title != "" {
		t.Errorf("索引里没有的曲目不该有曲名，得到 %q", samples[0].Title)
	}
}

// TestUtageDetailLimit 断言截断只影响明细，不影响未命中计数。
func TestUtageDetailLimit(t *testing.T) {
	scores := []model.Score{
		{MusicID: 100022, Level: model.LevelUtage},
		{MusicID: 100508, Level: model.LevelUtage},
		{MusicID: 100999, Level: model.LevelUtage},
	}

	samples, unmapped := utageDetail(scores, utageTestIndex(), 2)

	if len(samples) != 2 {
		t.Errorf("明细条数 = %d, 期望截断到 2", len(samples))
	}
	if unmapped != 2 {
		t.Errorf("未命中数 = %d, 期望仍为 2（不受截断影响）", unmapped)
	}
}

// TestUtageDetailNoUtage 断言没有宴谱时不产出明细。
func TestUtageDetailNoUtage(t *testing.T) {
	scores := []model.Score{{MusicID: 1001, Level: model.LevelMaster}}

	samples, unmapped := utageDetail(scores, utageTestIndex(), 0)

	if samples != nil || unmapped != 0 {
		t.Errorf("无宴谱时应为空，得到 %d 条 / 未命中 %d", len(samples), unmapped)
	}
	if hasUtage(scores) {
		t.Error("hasUtage 对普通成绩应返回 false")
	}
}
