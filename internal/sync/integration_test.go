//go:build integration

// 真实查分器上的只读验证：
//
//	MAI_FISH_TOKEN='...' go test -tags integration ./internal/sync/ -v
//
// 本文件刻意不做任何写入：往真实账号上传成绩会改变数据，必须由人明确发起，
// 手动流程见 README 的「同步验证」一节。
package sync

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// TestIntegrationMergePreservesRealRemoteMarks 用真实账号的现有成绩验证合并保住 FC/FS。
//
// 输入是「机台没给任何竞速标识」的成绩——这正是会把服务器标记清空的场景。
// 断言合并结果里，服务器上原本有标记的那些曲目，标记被原样保留。
func TestIntegrationMergePreservesRealRemoteMarks(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("MAI_FISH_TOKEN"))
	if token == "" {
		t.Skip("未设置 MAI_FISH_TOKEN，跳过需要真实凭证的验证")
	}

	hc, err := transport.New(transport.Options{Timeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("构造传输客户端失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 直接拉一份曲目数据：既建索引，也建一张 (歌名, 类型) → songId 的反查表。
	// 反查表只在这个测试里需要，因此不为了它去扩 chart 的公开 API。
	resp, err := hc.Get(ctx, chart.DefaultBaseURL+"/music_data", nil)
	if err != nil {
		t.Fatalf("拉取曲目数据失败: %v", err)
	}
	songs, err := chart.SongsFromMusicData(resp.Body)
	if err != nil {
		t.Fatalf("解析曲目数据失败: %v", err)
	}
	index := chart.NewIndex(songs)

	byTitleType := make(map[[2]string]int, len(songs))
	for _, song := range songs {
		byTitleType[[2]string{song.Title, song.Type}] = song.ID
	}

	syncer := NewDivingFish(Deps{HTTP: hc, Songs: index})

	remote, err := syncer.loadRemote(ctx, token)
	if err != nil {
		t.Fatalf("读取真实成绩失败: %v", err)
	}
	if len(remote) == 0 {
		t.Skip("该账号没有成绩，无法验证合并")
	}

	// 统计服务器上带竞速标识的条数，作为对比基准。
	withFC, withFS := 0, 0
	for _, r := range remote {
		if r.FC != "" {
			withFC++
		}
		if r.FS != "" {
			withFS++
		}
	}
	t.Logf("真实成绩 %d 条，带 fc 标记 %d 条，带 fs 标记 %d 条", len(remote), withFC, withFS)

	// 构造一批「机台没给竞速标识」的成绩，覆盖服务器上所有已记录曲目。
	scores := make([]model.Score, 0, len(remote))
	for _, r := range remote {
		id, ok := byTitleType[[2]string{r.Title, r.Type}]
		if !ok {
			continue
		}
		scores = append(scores, model.Score{
			MusicID:     id,
			Level:       model.LevelIndex(r.LevelIndex),
			Achievement: r.Achievements,
			DXScore:     r.DXScore,
			PlayCount:   1,
		})
	}
	if len(scores) == 0 {
		t.Fatal("没有一条真实成绩能映射回曲目，说明曲目匹配逻辑有问题")
	}

	merged := merge(remote, scores, index)
	t.Logf("机台条数 %d，待上传 %d 条，保留标记 %d 次，未知曲目跳过 %d 条",
		len(scores), len(merged.Records), merged.PreservedMarks, merged.SkippedUnknown)

	if merged.PreservedMarks == 0 {
		t.Fatalf("一条标记都没保留，而服务器上本有 fc %d 条、fs %d 条 —— 合并逻辑未生效", withFC, withFS)
	}

	// 逐条核对：服务器上有的标记，在待上传内容里必须还在。
	lost := 0
	for _, record := range merged.Records {
		for _, r := range remote {
			if r.Title != record.Title || r.Type != record.Type || r.LevelIndex != record.LevelIndex {
				continue
			}
			if r.FC != "" && record.FC != r.FC {
				lost++
				t.Errorf("%s [%s] %s: fc 由 %q 变成了 %q",
					r.Title, r.Type, model.LevelIndex(r.LevelIndex), r.FC, record.FC)
			}
			if r.FS != "" && record.FS != r.FS {
				lost++
				t.Errorf("%s [%s] %s: fs 由 %q 变成了 %q",
					r.Title, r.Type, model.LevelIndex(r.LevelIndex), r.FS, record.FS)
			}
			break
		}
	}
	if lost > 0 {
		t.Errorf("共有 %d 个标记在合并后丢失", lost)
	}
}
