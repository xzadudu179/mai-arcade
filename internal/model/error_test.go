package model

import (
	"errors"
	"testing"
)

// TestScoreIsUtage 断言宴谱的判定边界。
//
// 宴谱既不参与查分器同步也不计入 b50，边界写错会让宴谱混进正式成绩。
func TestScoreIsUtage(t *testing.T) {
	tests := []struct {
		name    string
		musicID int
		want    bool
	}{
		{"SD 谱", 8, false},
		{"DX 谱", 10030, false},
		{"宴谱前一号", UtageMusicIDBase - 1, false},
		{"宴谱起始号", UtageMusicIDBase, true},
		{"宴谱", 100508, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			score := Score{MusicID: tc.musicID}
			if got := score.IsUtage(); got != tc.want {
				t.Errorf("musicId=%d 的 IsUtage() = %v, 期望 %v", tc.musicID, got, tc.want)
			}
		})
	}
}

// TestLevelIndexValidAndString 断言难度取值的合法范围与名称。
//
// level=5 是宴谱档，必须被判为越界：上传时它无法映射到任何真实难度。
func TestLevelIndexValidAndString(t *testing.T) {
	tests := []struct {
		level     LevelIndex
		wantValid bool
		wantName  string
	}{
		{LevelBasic, true, "Basic"},
		{LevelAdvanced, true, "Advanced"},
		{LevelExpert, true, "Expert"},
		{LevelMaster, true, "Master"},
		{LevelReMaster, true, "Re:Master"},
		{5, false, "Unknown"},
		{200, false, "Unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.wantName, func(t *testing.T) {
			if got := tc.level.Valid(); got != tc.wantValid {
				t.Errorf("LevelIndex(%d).Valid() = %v, 期望 %v", tc.level, got, tc.wantValid)
			}
			if got := tc.level.String(); got != tc.wantName {
				t.Errorf("LevelIndex(%d).String() = %q, 期望 %q", tc.level, got, tc.wantName)
			}
		})
	}
}

// TestComboStatusDivingFish 断言连击竞速标识到水鱼取值的映射。
//
// 水鱼只接受 fc/fcp/ap/app，其余值会被服务端置空——映射错一个字母就等于丢标记。
func TestComboStatusDivingFish(t *testing.T) {
	tests := []struct {
		status ComboStatus
		want   string
	}{
		{ComboNone, ""},
		{ComboFC, "fc"},
		{ComboFCPlus, "fcp"},
		{ComboAP, "ap"},
		{ComboAPPlus, "app"},
		{99, ""},
	}

	for _, tc := range tests {
		t.Run(tc.want+"/"+string(rune('0'+tc.status)), func(t *testing.T) {
			if got := tc.status.DivingFish(); got != tc.want {
				t.Errorf("ComboStatus(%d).DivingFish() = %q, 期望 %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestSyncStatusDivingFish 断言同步竞速标识到水鱼取值的映射。
func TestSyncStatusDivingFish(t *testing.T) {
	tests := []struct {
		status SyncStatus
		want   string
	}{
		{SyncNone, ""},
		{SyncFS, "fs"},
		{SyncFSPlus, "fsp"},
		{SyncFSD, "fsd"},
		{SyncFSDPlus, "fsdp"},
		{SyncFullSync, "sync"},
		{99, ""},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.status.DivingFish(); got != tc.want {
				t.Errorf("SyncStatus(%d).DivingFish() = %q, 期望 %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestErrorKindExitCode 断言分类与退出码的对应关系符合 §8 的约定。
func TestErrorKindExitCode(t *testing.T) {
	tests := []struct {
		kind Kind
		want int
	}{
		{KindBusiness, 1},
		{KindNetwork, 2},
		{KindParam, 3},
	}

	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			if got := tc.kind.ExitCode(); got != tc.want {
				t.Errorf("Kind(%d).ExitCode() = %d, 期望 %d", tc.kind, got, tc.want)
			}
		})
	}
}

// TestErrorIsMatchesBySentinelAndKind 断言 errors.Is 的两种匹配方式都能用。
//
// 分类匹配让 CLI 只需一条 errors.Is 就能定退出码；具名匹配让调用方能精确识别单个错误。
func TestErrorIsMatchesBySentinelAndKind(t *testing.T) {
	named := Errorf(KindParam, CodeSGIDFormat, "code-1", "描述", "建议", nil)

	if !errors.Is(named, ErrParam) {
		t.Error("具名错误应当能被分类哨兵命中")
	}
	if errors.Is(named, ErrBusiness) {
		t.Error("具名错误不应被其他分类命中")
	}

	sameSentinel := Errorf(KindParam, CodeSGIDFormat, "code-2", "另一个实例", "", nil)
	if !errors.Is(sameSentinel, named) {
		t.Error("相同 Sentinel 的错误应当互相匹配")
	}

	other := Errorf(KindParam, Code("other"), "", "", "", nil)
	if errors.Is(other, named) {
		t.Error("不同 Sentinel 的错误不应互相匹配")
	}
}

// TestErrorUnwrap 断言包装链被保留。
func TestErrorUnwrap(t *testing.T) {
	cause := errors.New("底层原因")
	wrapped := Errorf(KindNetwork, Code("x"), "", "外层", "", cause)

	if !errors.Is(wrapped, cause) {
		t.Error("应当能穿透到下层错误")
	}
	if got := wrapped.Error(); got == "" {
		t.Error("错误描述不应为空")
	}
}

// TestErrorPrefersSentinelWhenMsgEmpty 断言只有 Sentinel 时描述仍然可用。
func TestErrorPrefersSentinelWhenMsgEmpty(t *testing.T) {
	err := &Error{Kind: KindParam, Sentinel: Code("only_sentinel")}
	if got := err.Error(); got != "only_sentinel" {
		t.Errorf("Error() = %q, 期望回落到 Sentinel", got)
	}
}
