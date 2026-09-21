package sync

import (
	"context"
	"testing"

	"github.com/xzadudu179/maimai-arcade/internal/model"
)

// 本文件是 §9「扩展点验收」的凭据：
// 新增一个查分器实现，只新增了这一个文件、只加了下面 init 里的一行注册，
// 既有文件零改动。如果哪天加站点必须去动 syncer.go 或 service，这条验证就会失败。

// fakeSyncer 是一个不联网的假查分器，用于证明扩展机制可用。
type fakeSyncer struct {
	// 记录最后一次上传的内容，供测试断言。
	lastScores     []model.Score
	lastCredential string
}

// Name 返回注册名。
func (f *fakeSyncer) Name() string { return "fake" }

// Upload 只记录调用参数。
func (f *fakeSyncer) Upload(_ context.Context, scores []model.Score, credential string) error {
	f.lastScores = scores
	f.lastCredential = credential
	return nil
}

// 假查分器的实例，便于测试直接取用。
var theFakeSyncer = &fakeSyncer{}

func init() {
	Register("fake", func(Deps) Syncer { return theFakeSyncer })
}

// TestFakeSyncerRegisteredWithoutTouchingExistingFiles 断言假查分器注册后立即可用。
func TestFakeSyncerRegisteredWithoutTouchingExistingFiles(t *testing.T) {
	syncer, err := New("fake", Deps{})
	if err != nil {
		t.Fatalf("按名取假查分器失败: %v", err)
	}
	if syncer.Name() != "fake" {
		t.Errorf("Name() = %q, 期望 fake", syncer.Name())
	}

	scores := []model.Score{{MusicID: 1, Level: model.LevelBasic, Achievement: 100.0, PlayCount: 1}}
	if err := syncer.Upload(context.Background(), scores, "cred"); err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if len(theFakeSyncer.lastScores) != 1 || theFakeSyncer.lastCredential != "cred" {
		t.Errorf("假查分器未收到上传内容: %+v", theFakeSyncer)
	}

	// 新实现必须同时出现在可用取值列表里，CLI 的 --site 提示才会带上它。
	found := false
	for _, name := range Names() {
		if name == "fake" {
			found = true
		}
	}
	if !found {
		t.Errorf("Names() 应包含 fake，实际 %v", Names())
	}
}

// TestEveryRegisteredSyncerNameMatchesKey 断言注册键与实现自报的名字一致。
//
// 两者不一致时 CLI 的 --site 会取到一个名字、错误信息里又显示另一个，排查成本很高。
func TestEveryRegisteredSyncerNameMatchesKey(t *testing.T) {
	for _, name := range Names() {
		syncer, err := New(name, Deps{HTTP: newTestHTTP(t)})
		if err != nil {
			t.Errorf("按名 %q 取实现失败: %v", name, err)
			continue
		}
		if syncer == nil {
			t.Errorf("按名 %q 取到 nil 实现", name)
			continue
		}
		if got := syncer.Name(); got != name {
			t.Errorf("注册键 %q 与 Name() %q 不一致", name, got)
		}
	}
}
