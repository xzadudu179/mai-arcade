package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// FaultClass 是自检判定出的故障类别，取值与退出码约定一致。
type FaultClass string

// 四类故障判定，外加成功。
const (
	// ClassOK 表示该目标自检通过。
	ClassOK FaultClass = "ok"

	// ClassNetwork 表示 TCP/TLS 建连失败：网络不通、代理或防火墙问题。
	ClassNetwork FaultClass = "network"

	// ClassBlocked 表示 TCP+TLS 正常但响应 0 字节：出口 IP 被华立阻断。
	ClassBlocked FaultClass = "blocked"

	// ClassParams 表示收到响应但解不开：协议参数版本不匹配。
	ClassParams FaultClass = "params"

	// ClassVersionStale 表示链路通、但配置的协议版本已不被服务端接受，
	// 而另一个版本可用。
	//
	// 这一类比文档里的四类更细：版本被弃用与出口 IP 被阻断在传输层完全同形
	// （都是 HTTP 200 + 0 字节），只有逐个尝试版本才能区分，而两者的处置方式相反。
	ClassVersionStale FaultClass = "version_stale"

	// ClassBusiness 表示收到业务错误码：协议通了，问题在账号或参数取值。
	ClassBusiness FaultClass = "business"
)

// exitCode 返回该类别对应的进程退出码。
func (c FaultClass) exitCode() int {
	switch c {
	case ClassOK:
		return 0
	case ClassBusiness:
		return model.KindBusiness.ExitCode()
	case ClassNetwork, ClassBlocked:
		return model.KindNetwork.ExitCode()
	case ClassParams, ClassVersionStale:
		return model.KindParam.ExitCode()
	default:
		return model.KindParam.ExitCode()
	}
}

// TargetVerdict 是单个目标的自检结论。
type TargetVerdict struct {
	// Target 是目标名，如 title / aime。
	Target string `json:"target"`

	// Class 是故障类别。
	Class FaultClass `json:"class"`

	// OK 报告是否通过。
	OK bool `json:"ok"`

	// Detail 是观察到的事实，如状态码、响应字节数。
	Detail string `json:"detail"`

	// Hint 是排查建议。
	Hint string `json:"hint,omitempty"`

	// AcceptedVersion 是实测可用的协议版本；仅 title 目标会填。
	AcceptedVersion string `json:"acceptedVersion,omitempty"`
}

// ProbeResult 是一次自检的完整结论。
type ProbeResult struct {
	// Version 是本次使用的协议版本。
	Version string `json:"version"`

	// Verdicts 按固定顺序列出各目标的结论。
	Verdicts []TargetVerdict `json:"verdicts"`

	// ExitCode 是综合退出码，取值符合 §8 的 0/1/2/3 约定。
	ExitCode int `json:"exitCode"`
}

// Probe 执行连通性自检，无需任何凭证。
//
// 它回答四个问题：网络通不通、出口 IP 是否被阻断、协议参数版本对不对、业务侧是否正常。
// 除机台外也会探测 AimeDB，因为「换账号」是链路的第一步，它不通整条链路都走不下去。
func (s *Service) Probe(ctx context.Context) (ProbeResult, error) {
	result := ProbeResult{Version: s.version.Encoding}

	title := s.probeTitle(ctx)
	aime := s.probeAime(ctx)
	result.Verdicts = []TargetVerdict{title, aime}
	result.ExitCode = combine(title.Class, aime.Class).exitCode()
	return result, nil
}

// probeCandidates 返回自检要尝试的协议版本：配置的版本优先，再跟上候选新版本。
func (s *Service) probeCandidates() []string {
	configured := s.Version()
	out := []string{configured}
	for _, name := range versionProbeOrder {
		if name != configured {
			out = append(out, name)
		}
	}
	return out
}

// versionAttempt 是一次带版本的探活结果。
type versionAttempt struct {
	version string
	err     error
}

// probeTitle 探测标题服务器：先确认能建连，再逐个尝试协议版本。
//
// 必须逐个试的原因：服务端对「不再接受的协议版本」与「被阻断的出口 IP」都回
// HTTP 200 + 0 字节，两者在传输层无法区分，而处置方式恰好相反——
// 前者换版本即可，后者换 IP 才有用。
func (s *Service) probeTitle(ctx context.Context) TargetVerdict {
	base := s.opts.TitleBaseURL
	if base == "" {
		base = protocol.DefaultTitleBaseURL
	}

	if err := s.hc.HostReachable(ctx, base); err != nil {
		return TargetVerdict{
			Target: "title", Class: ClassNetwork, OK: false,
			Detail: "TCP/TLS 建连失败: " + err.Error(),
			Hint:   "检查代理与防火墙；确认能解析并连上 maimai-gm.wahlap.com:42081",
		}
	}

	var attempts []versionAttempt
	for _, name := range s.probeCandidates() {
		version, err := protocol.LookupVersion(name)
		if err != nil {
			continue
		}
		client, err := protocol.NewTitleClient(protocol.TitleOptions{
			HTTP:    s.hc,
			Version: version,
			BaseURL: s.opts.TitleBaseURL,
			Logger:  s.logger,
		})
		if err != nil {
			attempts = append(attempts, versionAttempt{version: name, err: err})
			continue
		}

		pingErr := client.Ping(ctx)
		attempts = append(attempts, versionAttempt{version: name, err: pingErr})

		if pingErr == nil {
			break
		}
		// 网络类或业务类失败换版本也解决不了，继续试只是白打上游。
		if !isVersionDiagnosable(pingErr) {
			break
		}
	}
	return titleVerdictFromAttempts(s.Version(), attempts)
}

// isVersionDiagnosable 报告该错误是否值得换一个协议版本再试。
func isVersionDiagnosable(err error) bool {
	return errors.Is(err, protocol.ErrEmptyResponse) ||
		errors.Is(err, protocol.ErrDecrypt) ||
		errors.Is(err, protocol.ErrVersionMismatch)
}

// titleVerdictFromAttempts 依据各版本的探活结果给出判定。
func titleVerdictFromAttempts(configured string, attempts []versionAttempt) TargetVerdict {
	v := TargetVerdict{Target: "title"}
	if len(attempts) == 0 {
		v.Class = ClassParams
		v.Detail = "没有任何可用的协议版本参数"
		v.Hint = "检查 protocol.Versions 参数表"
		return v
	}

	// 有版本能解开：链路是通的。
	var accepted string
	configuredWorks := false
	for _, attempt := range attempts {
		if attempt.err != nil {
			continue
		}
		if accepted == "" {
			accepted = attempt.version
		}
		if attempt.version == configured {
			configuredWorks = true
		}
	}

	if accepted != "" {
		v.AcceptedVersion = accepted
		if configuredWorks {
			v.Class, v.OK = ClassOK, true
			v.Detail = fmt.Sprintf("Ping 成功，响应可解密（版本 %s）", accepted)
			return v
		}
		// 链路通，但配置的版本已不被接受——最容易被误判成「IP 被阻断」的情形。
		v.Class = ClassVersionStale
		v.Detail = fmt.Sprintf("配置的版本 %s 拿不到可用响应，而版本 %s 正常；%s",
			configured, accepted, summarizeAttempts(attempts))
		v.Hint = fmt.Sprintf("协议版本已更新：加 --version %s，或用 --auto-version 自动选用；"+
			"这不是出口 IP 问题，换网络不会改善", accepted)
		return v
	}

	// 没有任何版本能解开：再看失败形态。
	v.Detail = summarizeAttempts(attempts)
	allEmpty := true
	for _, attempt := range attempts {
		if !errors.Is(attempt.err, protocol.ErrEmptyResponse) {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		v.Class = ClassBlocked
		v.Hint = "所有协议版本都拿不到响应体。三种成因按可能性排序：" +
			"① 短时间内请求过多触发了封禁（等十几分钟再试，别继续打）；" +
			"② 出口 IP 被阻断（换家宽 IP / 重启光猫 / 等 48–72 小时）；" +
			"③ 协议参数已被再次更换（确认是否有更新版本）"
		return v
	}

	return titleVerdictFromError(attempts[0].err)
}

// summarizeAttempts 把各版本的探活结果压成一行，便于直接读结论。
func summarizeAttempts(attempts []versionAttempt) string {
	parts := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		outcome := "正常"
		switch {
		case attempt.err == nil:
		case errors.Is(attempt.err, protocol.ErrEmptyResponse):
			outcome = "0 字节"
		case errors.Is(attempt.err, protocol.ErrDecrypt), errors.Is(attempt.err, protocol.ErrVersionMismatch):
			outcome = "解密失败"
		default:
			outcome = attempt.err.Error()
		}
		parts = append(parts, fmt.Sprintf("%s=%s", attempt.version, outcome))
	}
	return strings.Join(parts, "，")
}

// titleVerdictFromError 把单个 Ping 错误翻译成故障类别。
//
// 判定顺序有讲究：0 字节响应本身属于网络分类，但它的含义可能是「被阻断」，
// 也可能是「版本不被接受」，所以必须先于分类判断单独识别。
func titleVerdictFromError(err error) TargetVerdict {
	v := TargetVerdict{Target: "title"}
	switch {
	case err == nil:
		v.Class, v.OK = ClassOK, true
		v.Detail = "Ping 成功，响应可解密"
	case errors.Is(err, protocol.ErrEmptyResponse):
		v.Class = ClassBlocked
		v.Detail = err.Error()
		v.Hint = "0 字节响应有三种成因，按处置代价从低到高：① 协议版本已不被接受 → 换 --version；" +
			"② 短时间内请求过多被临时封禁 → 等十几分钟，期间别再打；" +
			"③ 出口 IP 被阻断 → 换家宽 IP / 重启光猫 / 等 48–72 小时"
	case errors.Is(err, protocol.ErrDecrypt), errors.Is(err, protocol.ErrVersionMismatch):
		v.Class = ClassParams
		v.Detail = err.Error()
		v.Hint = "协议参数版本不匹配，用 --version 换一个版本重试（如 --version 1.55）"
	case protocol.IsKind(err, protocol.KindNetwork):
		v.Class = ClassNetwork
		v.Detail = err.Error()
		v.Hint = "检查网络与代理出口"
	case protocol.IsKind(err, protocol.KindBusiness):
		v.Class = ClassBusiness
		v.Detail = err.Error()
		v.Hint = "协议已打通，问题在业务侧"
	default:
		v.Class = ClassBusiness
		v.Detail = err.Error()
	}
	return v
}

// probeAime 探测 AimeDB 并验证签名被服务端接受。
//
// 它用一个合成的二维码：签名只覆盖 chipID、时间戳与 commonKey，与二维码内容无关，
// 因此服务端只要返回「二维码过期」之类的业务码，就说明请求格式与签名算法都被接受了——
// 这正是不需要任何真实凭证就能完成的验证。
func (s *Service) probeAime(ctx context.Context) TargetVerdict {
	client, err := protocol.NewAimeClient(protocol.AimeOptions{
		HTTP:   s.hc,
		URL:    s.opts.AimeURL,
		Logger: s.logger,
	})
	if err != nil {
		return TargetVerdict{Target: "aime", Class: ClassParams, Detail: err.Error()}
	}

	synthetic, err := syntheticSGID(time.Now())
	if err != nil {
		return TargetVerdict{Target: "aime", Class: ClassParams, Detail: "构造合成二维码失败: " + err.Error()}
	}

	_, err = client.Exchange(ctx, synthetic)
	return aimeVerdictFromError(err)
}

func aimeVerdictFromError(err error) TargetVerdict {
	v := TargetVerdict{Target: "aime"}
	switch {
	case err == nil:
		// 合成二维码理论上换不到账号；真换到了也不影响自检通过。
		v.Class, v.OK = ClassOK, true
		v.Detail = "AimeDB 可访问且请求被接受"
	case errors.Is(err, protocol.ErrAimeQRRejected):
		// 合成二维码必然查不到账号，服务端因此回「过期/不存在」。
		// 得到这个码恰恰说明请求格式与签名算法都被接受了——这正是自检要证明的事。
		v.Class, v.OK = ClassOK, true
		v.Detail = "AimeDB 可访问，请求格式与签名被接受（合成二维码按预期被判为不可用）: " + err.Error()
	case errors.Is(err, protocol.ErrAimeBadSignature):
		v.Class = ClassParams
		v.Detail = err.Error()
		v.Hint = "签名被拒：检查本机时区与 chipID（时间戳按本地时区生成）"
	case protocol.IsKind(err, protocol.KindNetwork):
		v.Class = ClassNetwork
		v.Detail = err.Error()
		v.Hint = "AimeDB 是明文 HTTP 接口；确认网络可直连 ai.sys-allnet.cn"
	case protocol.IsKind(err, protocol.KindBusiness):
		v.Class = ClassBusiness
		v.Detail = err.Error()
		v.Hint = "协议已打通，问题在业务侧"
	default:
		v.Class = ClassBusiness
		v.Detail = err.Error()
	}
	return v
}

// syntheticSGID 构造格式合法但不对应任何真实账号的二维码。
//
// 前缀与时间戳都是真的，只有签名段是占位符，因此它仍然能完整检验时间戳格式与签名算法。
func syntheticSGID(now time.Time) (protocol.SGID, error) {
	raw := "SGWCMAID" + now.Format("060102150405") + strings.Repeat("0", 64)
	return protocol.NewSGID(raw)
}

// combine 取两个目标里最严重的类别：
// 只在两边都通过时才算通过，否则优先呈现更「靠前」的故障（参数/版本 > 阻断/网络 > 业务）。
//
// 严重度表必须覆盖每一个类别：漏项会取到零值，把该故障当成「正常」，静默放宽判定。
func combine(a, b FaultClass) FaultClass {
	severity := map[FaultClass]int{
		ClassOK:           0,
		ClassBusiness:     1,
		ClassNetwork:      2,
		ClassBlocked:      2,
		ClassParams:       3,
		ClassVersionStale: 3,
	}
	worst := a
	if severity[b] > severity[worst] {
		worst = b
	}
	return worst
}

// VerifyResult 是 verify 子命令的输出：只描述字段结构，不含任何凭证。
type VerifyResult struct {
	// UserID 是机台账号。
	UserID int

	// ScoreCount 是拉到的成绩条数。
	ScoreCount int

	// WithCombo 是带连击竞速标识的条数。
	WithCombo int

	// WithSync 是带同步竞速标识的条数。
	WithSync int

	// ComboCounts 按 comboStatus 统计条数，键是枚举名。
	ComboCounts map[string]int

	// SyncCounts 按 syncStatus 统计条数，键是枚举名。
	SyncCounts map[string]int

	// LevelCounts 按难度统计条数，键是难度名。
	LevelCounts map[string]int

	// UtageCount 是宴谱条数。
	UtageCount int

	// Samples 是前若干条成绩的字段结构样例（脱敏，仅数值与枚举）。
	Samples []SampleScore

	// Warnings 透传会话期的非致命问题。
	Warnings []string
}

// SampleScore 是一条用于展示字段结构的成绩样例。
type SampleScore struct {
	MusicID     int     `json:"musicId"`
	Level       string  `json:"level"`
	Achievement float64 `json:"achievement"`
	DXScore     int     `json:"dxScore"`
	Combo       string  `json:"combo"`
	Sync        string  `json:"sync"`
	PlayCount   int     `json:"playCount"`
}

// sampleLimit 是 verify 展示的样例条数。
const sampleLimit = 5

// comboName / syncName 把枚举转成可读名，用于统计与展示。
func comboName(c model.ComboStatus) string {
	switch c {
	case model.ComboNone:
		return "none"
	case model.ComboFC:
		return "fc"
	case model.ComboFCPlus:
		return "fcp"
	case model.ComboAP:
		return "ap"
	case model.ComboAPPlus:
		return "app"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

func syncName(s model.SyncStatus) string {
	switch s {
	case model.SyncNone:
		return "none"
	case model.SyncFS:
		return "fs"
	case model.SyncFSPlus:
		return "fsp"
	case model.SyncFSD:
		return "fsd"
	case model.SyncFSDPlus:
		return "fsdp"
	case model.SyncFullSync:
		return "sync"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// Verify 走完整链路但不上传：用于确认能拿到全量成绩且含 comboStatus / syncStatus。
func (s *Service) Verify(ctx context.Context, sgid protocol.SGID) (VerifyResult, error) {
	session, err := s.Fetch(ctx, sgid)
	if err != nil {
		return VerifyResult{}, err
	}

	result := VerifyResult{
		UserID:      session.UserID,
		ScoreCount:  len(session.Scores),
		ComboCounts: map[string]int{},
		SyncCounts:  map[string]int{},
		LevelCounts: map[string]int{},
		Warnings:    session.Warnings,
	}

	for _, score := range session.Scores {
		result.ComboCounts[comboName(score.Combo)]++
		result.SyncCounts[syncName(score.Sync)]++
		result.LevelCounts[score.Level.String()]++
		if score.Combo != model.ComboNone {
			result.WithCombo++
		}
		if score.Sync != model.SyncNone {
			result.WithSync++
		}
		if score.IsUtage() {
			result.UtageCount++
		}
	}

	// 样例按 musicId 排序，保证同一份数据每次输出一致。
	ordered := append([]model.Score(nil), session.Scores...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].MusicID < ordered[j].MusicID })
	for i, score := range ordered {
		if i >= sampleLimit {
			break
		}
		result.Samples = append(result.Samples, SampleScore{
			MusicID:     score.MusicID,
			Level:       score.Level.String(),
			Achievement: score.Achievement,
			DXScore:     score.DXScore,
			Combo:       comboName(score.Combo),
			Sync:        syncName(score.Sync),
			PlayCount:   score.PlayCount,
		})
	}
	return result, nil
}
