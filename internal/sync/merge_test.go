package sync

import (
	"testing"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// testIndex 构造一份测试曲目索引。
func testIndex() *chart.Index {
	return chart.NewIndex([]chart.Song{
		{ID: 1001, Title: "Alea jacta est!", Type: "DX", DS: []float64{3, 7, 10, 13}},
		{ID: 1002, Title: "Link", Type: "SD", DS: []float64{2, 6, 9, 12}},
		// 同名不同曲：水鱼靠 title 区分，写错歌名就会匹配到另一首。
		{ID: 1003, Title: "Link(CoF)", Type: "SD", DS: []float64{2, 6, 9, 13}},
		{ID: 1004, Title: "Plain Song", Type: "SD", DS: []float64{1, 5, 8, 11}},
		// 宴谱：查分器里只有一个难度档，所以 ds 也只有一项。
		{ID: 100508, Title: "[協]恋愛裁判", Type: "DX", DS: []float64{13.0}},
	})
}

// TestCountMappedUtage 断言只数映射得到的宴谱，且不把普通成绩算进去。
func TestCountMappedUtage(t *testing.T) {
	scores := []model.Score{
		{MusicID: 100508, Level: model.LevelUtage, Achievement: 99.0},
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 99.0},
		{MusicID: 100999, Level: model.LevelUtage, Achievement: 99.0}, // 索引里没有这首
	}

	if got := CountMappedUtage(scores, testIndex()); got != 1 {
		t.Errorf("能映射的宴谱数 = %d, 期望 1", got)
	}
	if got := CountMappedUtage(scores, nil); got != 0 {
		t.Errorf("曲目索引为 nil 时 = %d, 期望 0", got)
	}
}

// TestMergePreservesRemoteMarks 断言机台拿不到 FC/FS 时保留服务器原值。
//
// 这是整个同步逻辑里最要紧的一条：fc/fs 传空会清空服务器已有标记，
// 而合并的输入（服务端现状）正是唯一的保底来源。
func TestMergePreservesRemoteMarks(t *testing.T) {
	remote := []remoteRecord{
		{
			Title: "Alea jacta est!", Type: "DX", LevelIndex: 3,
			Achievements: 100.1234, DXScore: 2600,
			FC: "ap", FS: "fsd",
		},
	}

	// 机台这条成绩既没有连击标识也没有同步标识。
	scores := []model.Score{
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5, DXScore: 2711, PlayCount: 3},
	}

	got := merge(remote, scores, testIndex())

	if len(got.Records) != 1 {
		t.Fatalf("待上传条数 = %d, 期望 1", len(got.Records))
	}
	record := got.Records[0]
	if record.FC != "ap" {
		t.Errorf("fc = %q, 期望保留服务器原值 \"ap\"", record.FC)
	}
	if record.FS != "fsd" {
		t.Errorf("fs = %q, 期望保留服务器原值 \"fsd\"", record.FS)
	}
	if record.Achievements != 100.5 {
		t.Errorf("achievements = %v, 期望取机台的新值 100.5", record.Achievements)
	}
	if record.DXScore != 2711 {
		t.Errorf("dxScore = %d, 期望取较大值 2711", record.DXScore)
	}
	if got.PreservedMarks != 2 {
		t.Errorf("保留标记数 = %d, 期望 2（fc 与 fs 各一次）", got.PreservedMarks)
	}
}

// TestMergePrefersMachineMarksWhenPresent 断言机台给了标记时以机台为准。
func TestMergePrefersMachineMarksWhenPresent(t *testing.T) {
	remote := []remoteRecord{
		{Title: "Alea jacta est!", Type: "DX", LevelIndex: 3, Achievements: 100.0, FC: "fc", FS: "fs"},
	}
	scores := []model.Score{
		{
			MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5,
			Combo: model.ComboAPPlus, Sync: model.SyncFullSync, PlayCount: 1,
		},
	}

	got := merge(remote, scores, testIndex())

	record := got.Records[0]
	if record.FC != "app" {
		t.Errorf("fc = %q, 期望机台的 \"app\"", record.FC)
	}
	if record.FS != "sync" {
		t.Errorf("fs = %q, 期望机台的 \"sync\"", record.FS)
	}
	if got.PreservedMarks != 0 {
		t.Errorf("保留标记数 = %d, 期望 0", got.PreservedMarks)
	}
}

// TestMergeDoesNotCrossCharts 断言不同曲目、不同难度之间不会互相串数据。
func TestMergeDoesNotCrossCharts(t *testing.T) {
	remote := []remoteRecord{
		// 同名 SD 曲的 Master 有标记。
		{Title: "Link", Type: "SD", LevelIndex: 3, Achievements: 100.0, FC: "ap", FS: "sync"},
		// 同名不同曲 Link(CoF) 也有标记。
		{Title: "Link(CoF)", Type: "SD", LevelIndex: 3, Achievements: 100.0, FC: "fc"},
	}

	scores := []model.Score{
		// 机台上报的是 Link 的 Expert 难度：与远端 Master 不是同一条。
		{MusicID: 1002, Level: model.LevelExpert, Achievement: 99.5, PlayCount: 1},
		// 机台上报 Link(CoF) 的 Master：应当命中远端同名不同曲的那条。
		{MusicID: 1003, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
	}

	got := merge(remote, scores, testIndex())

	if len(got.Records) != 2 {
		t.Fatalf("待上传条数 = %d, 期望 2", len(got.Records))
	}

	byTitle := map[string]uploadRecord{}
	for _, r := range got.Records {
		byTitle[r.Title] = r
	}

	linkExpert := byTitle["Link"]
	if linkExpert.LevelIndex != int(model.LevelExpert) {
		t.Fatalf("Link 的难度 = %d, 期望 Expert", linkExpert.LevelIndex)
	}
	if linkExpert.FC != "" || linkExpert.FS != "" {
		t.Errorf("Link 的 Expert 在远端没有记录，不应带上标记: fc=%q fs=%q", linkExpert.FC, linkExpert.FS)
	}

	cof := byTitle["Link(CoF)"]
	if cof.FC != "fc" {
		t.Errorf("Link(CoF) 的 fc = %q, 期望保留 \"fc\"", cof.FC)
	}
	if got.RemoteOnly != 1 {
		t.Errorf("服务器独有数 = %d, 期望 1（Link 的 Master 机台未上报）", got.RemoteOnly)
	}
}

// TestMergeSkipsUnmappableScores 断言越界难度与未知曲目被跳过。
func TestMergeSkipsUnmappableScores(t *testing.T) {
	scores := []model.Score{
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		{MusicID: 9999, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 1},
		{MusicID: 1001, Level: model.LevelIndex(9), Achievement: 100.5, PlayCount: 1},
		// 宴谱档的难度套在普通曲目上：曲目不是宴谱，就没有折算的理由，只能跳过。
		{MusicID: 1001, Level: model.LevelUtage, Achievement: 100.5, PlayCount: 1},
	}

	got := merge(nil, scores, testIndex())

	if len(got.Records) != 1 {
		t.Fatalf("待上传条数 = %d, 期望 1", len(got.Records))
	}
	if got.Utage != 0 {
		t.Errorf("宴谱数 = %d, 期望 0", got.Utage)
	}
	if got.SkippedUnknown != 1 {
		t.Errorf("未知曲目跳过数 = %d, 期望 1", got.SkippedUnknown)
	}
	if got.SkippedInvalidLevel != 2 {
		t.Errorf("越界难度跳过数 = %d, 期望 2（level=9 与套在普通曲目上的 level=5）", got.SkippedInvalidLevel)
	}
}

// TestMergeSyncsUtageAsLevelZero 断言宴谱按折算后的难度 0 上传。
//
// 机台把宴谱报成 level=5，而查分器只认 0..4 且每首宴谱只有一个难度档：
// 不折算就会被服务端当作不存在的难度丢掉，折算成别的值则会写到错的难度上。
func TestMergeSyncsUtageAsLevelZero(t *testing.T) {
	scores := []model.Score{
		{MusicID: 100508, Level: model.LevelUtage, Achievement: 99.5, DXScore: 2100,
			Combo: model.ComboFC, Sync: model.SyncFS},
	}

	got := merge(nil, scores, testIndex())

	if len(got.Records) != 1 {
		t.Fatalf("待上传条数 = %d, 期望 1", len(got.Records))
	}
	record := got.Records[0]
	if record.LevelIndex != int(model.UtageLevelIndex) {
		t.Errorf("levelIndex = %d, 期望折算出 %d", record.LevelIndex, model.UtageLevelIndex)
	}
	if record.Title != "[協]恋愛裁判" || record.Type != "DX" {
		t.Errorf("title/type = %q/%q, 期望按 musicId 解析出宴谱曲名", record.Title, record.Type)
	}
	if record.FC != "fc" || record.FS != "fs" {
		t.Errorf("fc/fs = %q/%q, 期望宴谱的竞速标识照样带上", record.FC, record.FS)
	}
	if got.Utage != 1 {
		t.Errorf("宴谱数 = %d, 期望 1", got.Utage)
	}
}

// TestMergeUtageKeyAlignsWithRemote 断言宴谱的合并键与服务器上的 level_index=0 对得上。
//
// 折算错了不会立刻报错，只会静默多出一条记录：键对不上时服务器原有的 fc/fs 就保不住，
// 这正是合并最不能出的错。
func TestMergeUtageKeyAlignsWithRemote(t *testing.T) {
	remote := []remoteRecord{
		{Title: "[協]恋愛裁判", Type: "DX", LevelIndex: 0, Achievements: 99.9, FC: "ap", FS: "sync"},
	}
	scores := []model.Score{
		{MusicID: 100508, Level: model.LevelUtage, Achievement: 100.2, DXScore: 2000},
	}

	got := merge(remote, scores, testIndex())

	if len(got.Records) != 1 {
		t.Fatalf("待上传条数 = %d, 期望 1", len(got.Records))
	}
	record := got.Records[0]
	if record.FC != "ap" || record.FS != "sync" {
		t.Errorf("fc/fs = %q/%q, 期望保留服务器原值", record.FC, record.FS)
	}
	if got.RemoteOnly != 0 {
		t.Errorf("服务器独有数 = %d, 期望 0（键对齐后这条宴谱应被认作已上报）", got.RemoteOnly)
	}
}

// TestMergeDeduplicatesSameChart 断言同一谱面只上传一次。
func TestMergeDeduplicatesSameChart(t *testing.T) {
	scores := []model.Score{
		{MusicID: 1004, Level: model.LevelBasic, Achievement: 99.0, PlayCount: 1},
		{MusicID: 1004, Level: model.LevelBasic, Achievement: 99.0, PlayCount: 2},
	}

	got := merge(nil, scores, testIndex())

	if len(got.Records) != 1 {
		t.Fatalf("待上传条数 = %d, 期望 1", len(got.Records))
	}
}

// TestMergeNeverLowersRemoteScores 断言合并不回退服务器已有的成绩。
func TestMergeNeverLowersRemoteScores(t *testing.T) {
	remote := []remoteRecord{
		{Title: "Plain Song", Type: "SD", LevelIndex: 2, Achievements: 100.8, DXScore: 3000},
	}
	// 机台这次给的分更低（例如数据异常）。
	scores := []model.Score{
		{MusicID: 1004, Level: model.LevelExpert, Achievement: 95.0, DXScore: 100, PlayCount: 1},
	}

	got := merge(remote, scores, testIndex())

	record := got.Records[0]
	if record.Achievements != 100.8 {
		t.Errorf("achievements = %v, 期望保留较高的 100.8", record.Achievements)
	}
	if record.DXScore != 3000 {
		t.Errorf("dxScore = %d, 期望保留较高的 3000", record.DXScore)
	}
}

// TestMergeRoundsAchievementToFourDecimals 断言达成率被截到 4 位小数。
//
// 浮点误差会把 100.5 写成 100.49999999，回传后与机台显示不一致。
func TestMergeRoundsAchievementToFourDecimals(t *testing.T) {
	remote := []remoteRecord{
		{Title: "Plain Song", Type: "SD", LevelIndex: 3, Achievements: 100.00001},
	}
	scores := []model.Score{
		{MusicID: 1004, Level: model.LevelMaster, Achievement: 99.123456789, PlayCount: 1},
	}

	got := merge(remote, scores, testIndex())

	for _, r := range got.Records {
		if r.Achievements != 100.0 {
			t.Errorf("achievements = %v, 期望 100（99.123456789 与 100.00001 取较大后截断）", r.Achievements)
		}
	}
}

// TestParseRecordsBodyAcceptsBothShapes 断言对象包装与裸数组两种响应都能解析。
//
// 官方端点历史上两种形状都出现过，只支持一种会在换部署时直接失败。
func TestParseRecordsBodyAcceptsBothShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"对象包装", `{"records":[{"song_id":1,"title":"A","type":"SD","level_index":3,"achievements":100.5,"fc":"ap","fs":"fs"}]}`, 1},
		{"裸数组", `[{"song_id":1,"title":"A","type":"SD","level_index":3,"achievements":100.5,"fc":"ap","fs":"fs"}]`, 1},
		{"对象包装但空", `{"records":[]}`, 0},
		{"裸空数组", `[]`, 0},
		{"带前后空白", "\n\t [{\"title\":\"A\"}] \n", 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRecordsBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("条数 = %d, 期望 %d", len(got), tc.want)
			}
		})
	}
}

// TestParseRecordsBodyRejectsGarbage 断言非 JSON 响应报错而不是静默当空处理。
//
// 静默当空会让后续上传把服务器已有标记清空——这是最坏的失败方式。
func TestParseRecordsBodyRejectsGarbage(t *testing.T) {
	for _, body := range []string{"", "<html>", "null", "{", "{\"records\":\"x\"}"} {
		if _, err := parseRecordsBody([]byte(body)); err == nil {
			t.Errorf("body=%q 应当报错", body)
		}
	}
}

// TestComboAndSyncMapToAcceptedValues 断言上传的竞速标识只落在水鱼接受的取值集合里。
//
// 不在集合里的值会被服务端置空，等于把成绩上的标记抹掉。
func TestComboAndSyncMapToAcceptedValues(t *testing.T) {
	acceptedFC := map[string]bool{"": true, "fc": true, "fcp": true, "ap": true, "app": true}
	acceptedFS := map[string]bool{"": true, "fs": true, "fsp": true, "fsd": true, "fsdp": true, "sync": true}

	for i := 0; i < 16; i++ {
		combo := model.ComboStatus(i)
		if got := combo.DivingFish(); !acceptedFC[got] {
			t.Errorf("ComboStatus(%d) 映射出 %q，不在水鱼接受的取值内", i, got)
		}
		sync := model.SyncStatus(i)
		if got := sync.DivingFish(); !acceptedFS[got] {
			t.Errorf("SyncStatus(%d) 映射出 %q，不在水鱼接受的取值内", i, got)
		}
	}
}
