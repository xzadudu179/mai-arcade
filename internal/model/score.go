// Package model 存放跨层共享的纯数据模型：机台成绩、难度与竞速枚举，以及查分器侧的字符串映射。
//
// 它是依赖图上的叶子：protocol 生产这些值，sync 与 rating 消费它们，三方都不必互相依赖。
// 本包不含 IO、不含协议细节、不含网络。
package model

// UtageMusicIDBase 是宴会场谱面的 musicId 起点；宴谱不参与查分器同步，也不计入 b50。
const UtageMusicIDBase = 100000

// LevelIndex 是谱面难度，取值与机台 level 字段一致：0=Basic … 4=Re:Master。
type LevelIndex uint8

// 难度取值。
const (
	LevelBasic LevelIndex = iota
	LevelAdvanced
	LevelExpert
	LevelMaster
	LevelReMaster
)

var levelNames = [...]string{"Basic", "Advanced", "Expert", "Master", "Re:Master"}

// String 返回难度的英文名；越界返回 "Unknown"。
func (l LevelIndex) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return "Unknown"
}

// Valid 报告难度是否落在 0..4 内；宴谱的 level=5 会返回 false。
func (l LevelIndex) Valid() bool { return int(l) < len(levelNames) }

// ComboStatus 是机台 comboStatus 字段：连击竞速标识。
type ComboStatus uint8

// 连击竞速标识取值，与机台 comboStatus 一致。
const (
	ComboNone ComboStatus = iota
	ComboFC
	ComboFCPlus
	ComboAP
	ComboAPPlus
)

// DivingFish 返回水鱼 fc 字段的取值；无标记时返回空串。
//
// 水鱼只接受 fc/fcp/ap/app，其余值会被服务端置空——这正是同步必须做合并的原因。
func (c ComboStatus) DivingFish() string {
	switch c {
	case ComboFC:
		return "fc"
	case ComboFCPlus:
		return "fcp"
	case ComboAP:
		return "ap"
	case ComboAPPlus:
		return "app"
	default:
		return ""
	}
}

// SyncStatus 是机台 syncStatus 字段：同步竞速标识。
type SyncStatus uint8

// 同步竞速标识取值，与机台 syncStatus 一致。
const (
	SyncNone SyncStatus = iota
	SyncFS
	SyncFSPlus
	SyncFSD
	SyncFSDPlus
	SyncFullSync
)

// DivingFish 返回水鱼 fs 字段的取值；无标记时返回空串。
//
// 水鱼只接受 sync/fs/fsp/fsd/fsdp，其余值会被服务端置空。
func (s SyncStatus) DivingFish() string {
	switch s {
	case SyncFS:
		return "fs"
	case SyncFSPlus:
		return "fsp"
	case SyncFSD:
		return "fsd"
	case SyncFSDPlus:
		return "fsdp"
	case SyncFullSync:
		return "sync"
	default:
		return ""
	}
}

// Score 是归一化后的机台成绩：achievement 已从「×10000 的整数」还原成百分比。
type Score struct {
	MusicID     int
	Level       LevelIndex
	Achievement float64
	DXScore     int
	Combo       ComboStatus
	Sync        SyncStatus
	PlayCount   int
}

// IsUtage 报告该成绩是否为宴会场谱面。
func (s Score) IsUtage() bool { return s.MusicID >= UtageMusicIDBase }

// Key 是成绩在查分器侧的唯一定位：歌名 + 类型 + 难度。水鱼以歌名匹配曲目而非 song_id，
// 因此这里也必须以 trio 作为映射键。
type Key struct {
	Title      string
	Type       string
	LevelIndex LevelIndex
}
