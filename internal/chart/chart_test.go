package chart

import (
	"testing"

	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// sampleMusicData 是水鱼 music_data 的响应样本，字段形状与真实接口一致。
const sampleMusicData = `[
 {"id":"8","title":"True Love Song","type":"SD","ds":[5.0,7.2,10.2,12.4],
  "level":["5","7","10","12"],"basic_info":{"title":"True Love Song","from":"maimai","is_new":false}},
 {"id":"17","title":"Future","type":"SD","ds":[7.0,7.8,9.5,10.7,12.7],
  "level":["7","7+","9","10+","12+"],"basic_info":{"title":"Future","from":"maimai","is_new":false}},
 {"id":"10030","title":"ネコ日和。","type":"DX","ds":[2.0,6.5,9.5,13.7],
  "level":["2","6","9","13+"],"basic_info":{"title":"ネコ日和。","from":"maimai でらっくす PRiSM PLUS","is_new":true}},
 {"id":"100018","title":"[協]Love You","type":"DX","ds":[12.0],
  "level":["12?"],"basic_info":{"title":"[協]Love You","from":"maimai でらっくす BUDDiES","is_new":false}}
]`

// TestSongsFromMusicData 断言曲目数据被正确解析，含字符串 id 与单难度宴谱。
func TestSongsFromMusicData(t *testing.T) {
	songs, err := SongsFromMusicData([]byte(sampleMusicData))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(songs) != 4 {
		t.Fatalf("曲目数 = %d, 期望 4", len(songs))
	}

	index := NewIndex(songs)
	if index.Len() != 4 {
		t.Errorf("索引大小 = %d, 期望 4", index.Len())
	}

	tests := []struct {
		name      string
		musicID   int
		level     model.LevelIndex
		wantDS    float64
		wantDSOK  bool
		wantNew   bool
		wantTitle string
	}{
		{"SD 谱的 Master", 8, model.LevelMaster, 12.4, true, false, "True Love Song"},
		{"SD 谱的 Re:Master 存在", 17, model.LevelReMaster, 12.7, true, false, "Future"},
		{"DX 谱的 Master", 10030, model.LevelMaster, 13.7, true, true, "ネコ日和。"},
		{"DX 谱的 Basic", 10030, model.LevelBasic, 2.0, true, true, "ネコ日和。"},
		{"SD 谱没有 Re:Master", 8, model.LevelReMaster, 0, false, false, "True Love Song"},
		{"宴谱只有一项定数", 100018, model.LevelBasic, 12.0, true, false, "[協]Love You"},
		{"宴谱第二档不存在", 100018, model.LevelAdvanced, 0, false, false, "[協]Love You"},
		{"未知曲目", 999999, model.LevelMaster, 0, false, false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds, ok := index.DS(tc.musicID, tc.level)
			if ok != tc.wantDSOK {
				t.Fatalf("DS 存在性 = %v, 期望 %v", ok, tc.wantDSOK)
			}
			if ok && ds != tc.wantDS {
				t.Errorf("DS = %v, 期望 %v", ds, tc.wantDS)
			}
			if got := index.IsNew(tc.musicID); got != tc.wantNew {
				t.Errorf("IsNew = %v, 期望 %v", got, tc.wantNew)
			}
			title, titleOK := index.Title(tc.musicID)
			if titleOK != (tc.wantTitle != "") {
				t.Fatalf("Title 存在性 = %v, 期望 %v", titleOK, tc.wantTitle != "")
			}
			if titleOK && title != tc.wantTitle {
				t.Errorf("Title = %q, 期望 %q", title, tc.wantTitle)
			}
		})
	}
}

// TestSongsFromMusicDataAcceptsNumericID 断言 id 为数字时也能解析。
//
// 官方给的是字符串，但不同部署或中间层可能给数字，只认一种会在换环境时失败。
func TestSongsFromMusicDataAcceptsNumericID(t *testing.T) {
	songs, err := SongsFromMusicData([]byte(`[{"id":10030,"title":"A","type":"DX","ds":[1]}]`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(songs) != 1 || songs[0].ID != 10030 {
		t.Errorf("数字 id 解析不符: %+v", songs)
	}
}

// TestSongsFromMusicDataRejectsGarbage 断言非 JSON 或格式不对时报错。
func TestSongsFromMusicDataRejectsGarbage(t *testing.T) {
	for _, body := range []string{"", "<html>", "{}", `[{"id":"abc"}]`} {
		if _, err := SongsFromMusicData([]byte(body)); err == nil {
			t.Errorf("body=%q 应当报错", body)
		}
	}
}

// TestNewIndexSkipsIncompleteSongs 断言缺关键字段的条目被丢弃。
//
// 缺歌名或类型的条目无法用于上传匹配，留在索引里只会让合并阶段多出无效记录。
func TestNewIndexSkipsIncompleteSongs(t *testing.T) {
	index := NewIndex([]Song{
		{ID: 1, Title: "好的", Type: "SD", DS: []float64{1}},
		{ID: 0, Title: "缺 id", Type: "SD"},
		{ID: 2, Title: "", Type: "SD"},
		{ID: 3, Title: "缺类型", Type: ""},
	})

	if index.Len() != 1 {
		t.Errorf("索引大小 = %d, 期望 1", index.Len())
	}
	if _, ok := index.Song(1); !ok {
		t.Error("完整条目应当保留")
	}
}

// TestIndexOnNilIsSafe 断言零值索引下查询不会 panic。
//
// 零值不可用是对调用方的提示，但不该以崩溃的形式呈现。
func TestIndexOnNilIsSafe(t *testing.T) {
	var index *Index

	if index.Len() != 0 {
		t.Error("nil 索引长度应为 0")
	}
	if _, ok := index.Song(1); ok {
		t.Error("nil 索引不应查到曲目")
	}
	if _, ok := index.DS(1, model.LevelMaster); ok {
		t.Error("nil 索引不应查到定数")
	}
	if index.IsNew(1) {
		t.Error("nil 索引不应报告新曲")
	}
	if _, ok := index.Title(1); ok {
		t.Error("nil 索引不应查到歌名")
	}
}

// TestSourceInterfaceSatisfied 断言 Index 满足各层声明的 Source 接口。
func TestSourceInterfaceSatisfied(t *testing.T) {
	index := NewIndex([]Song{{ID: 1, Title: "A", Type: "SD", DS: []float64{1}}})
	var source Source = index
	if _, ok := source.Song(1); !ok {
		t.Error("Source 接口应能查到曲目")
	}
}
