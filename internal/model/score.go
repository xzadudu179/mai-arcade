// Package model 存放跨层共享的纯数据模型：机台成绩、难度与竞速枚举，以及查分器侧的字符串映射。
//
// 它是依赖图上的叶子：protocol 生产这些值，sync 与 rating 消费它们，三方都不必互相依赖。
// 本包不含 IO、不含协议细节、不含网络。
package model

// UtageMusicIDBase 是宴会场谱面的 musicId 起点；宴谱会被同步到查分器，但不计入 b50。
const UtageMusicIDBase = 100000

// LevelIndex 是谱面难度，取值与机台 level 字段一致：0=Basic … 4=Re:Master，宴谱为 5。
type LevelIndex uint8

// 难度取值。
const (
	LevelBasic LevelIndex = iota
	LevelAdvanced
	LevelExpert
	LevelMaster
	LevelReMaster

	// LevelUtage 是机台给宴谱报的难度值，超出正常谱面的 0..4。
	LevelUtage
)

// UtageLevelIndex 是宴谱在查分器侧的难度索引。
//
// 机台把宴谱报成 LevelUtage，而查分器里每首宴谱只有一个难度档（只有一个 level / ds 项），
// 所以同步时统一折算到 0——照原值上传会因为难度不存在而被查分器丢弃。
const UtageLevelIndex LevelIndex = 0

var levelNames = [...]string{"Basic", "Advanced", "Expert", "Master", "Re:Master"}

// String 返回难度的英文名；宴谱返回 "Utage"，越界返回 "Unknown"。
func (l LevelIndex) String() string {
	if l == LevelUtage {
		return "Utage"
	}
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return "Unknown"
}

// Valid 报告难度是否为正常谱面的 0..4；宴谱的 LevelUtage 会返回 false。
//
// 判的是「能不能当作查分器难度索引直接用」，所以宴谱必须为 false，
// 折算工作交给 Score.ChartLevel。
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

// ChartLevel 返回该成绩在查分器里对应的难度索引。
//
// 正常谱面就是自身难度；宴谱折算为 UtageLevelIndex。查分器不认 LevelUtage，
// 凡是要拿难度去查曲目或上传的地方都必须经过这里。
func (s Score) ChartLevel() LevelIndex {
	if s.IsUtage() {
		return UtageLevelIndex
	}
	return s.Level
}

// Key 是成绩在查分器侧的唯一定位：歌名 + 类型 + 难度。水鱼以歌名匹配曲目而非 song_id，
// 因此这里也必须以 trio 作为映射键。
type Key struct {
	Title      string
	Type       string
	LevelIndex LevelIndex
}
