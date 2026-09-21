package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xzadudu179/maimai-arcade/internal/rating"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// 本文件注册全部操作。新增一个操作 = 写一个 Run 函数 + 在 init 里注册一行；
// 接入方式（HTTP / WebSocket / 将来的其它方式）不需要任何改动。

func init() {
	Register(Operation{
		Name:    "probe",
		Summary: "连通性自检：区分网络不通 / 出口 IP 被阻断 / 协议参数版本错 / 业务错，不需要凭证",
		Run:     runProbe,
	})

	Register(Operation{
		Name:    "verify",
		Summary: "用二维码拉取全量成绩并返回字段结构统计，不上传任何数据",
		Run:     runVerify,
	})

	Register(Operation{
		Name:               "sync",
		Summary:            "用二维码拉取全量成绩并同步到查分器（会改动外部数据，需服务端开启写权限）",
		Mutating:           true,
		RequiresCredential: true,
		Run:                runSync,
	})

	Register(Operation{
		Name:    "profile",
		Summary: "用二维码拉取账号资料：头像 / 姓名框 / 牌子 / 称号 / 评级",
		Run:     runProfile,
	})

	Register(Operation{
		Name:    "b50",
		Summary: "用二维码拉取成绩并计算 b50 与 rating",
		Run:     runB50,
	})

	Register(Operation{
		Name:    "version",
		Summary: "返回服务与协议的版本信息，用于客户端探活",
		Run:     runVersion,
	})
}

// runProbe 执行连通性自检。
func runProbe(ctx context.Context, deps Deps, _ json.RawMessage) (any, error) {
	return deps.Service.Probe(ctx)
}

// runVerify 拉取成绩并返回结构统计。
func runVerify(ctx context.Context, deps Deps, args json.RawMessage) (any, error) {
	sgid, err := requireSGID(args)
	if err != nil {
		return nil, err
	}
	return deps.Service.Verify(ctx, sgid)
}

// syncArgs 是 sync 操作的参数。
type syncArgs struct {
	// SGID 是玩家二维码。
	SGID string `json:"sgid"`

	// Site 是查分器名，省略时用默认查分器。
	Site string `json:"site"`

	// Credential 是目标查分器的凭证（水鱼为 Import-Token）。
	Credential string `json:"credential"`
}

// runSync 拉取成绩并同步到查分器。
func runSync(ctx context.Context, deps Deps, args json.RawMessage) (any, error) {
	var parsed syncArgs
	if err := decodeArgs(args, &parsed); err != nil {
		return nil, err
	}
	if parsed.Credential == "" {
		return nil, fmt.Errorf("%w: 缺少 credential 参数", ErrBadArguments)
	}

	sgid, err := requireSGID(args)
	if err != nil {
		return nil, err
	}
	return deps.Service.Sync(ctx, service.SyncRequest{
		SGID:       sgid,
		Site:       parsed.Site,
		Credential: parsed.Credential,
	})
}

// runProfile 拉取账号资料。
func runProfile(ctx context.Context, deps Deps, args json.RawMessage) (any, error) {
	sgid, err := requireSGID(args)
	if err != nil {
		return nil, err
	}
	return deps.Service.Profile(ctx, sgid)
}

// b50Args 是 b50 操作的参数。
type b50Args struct {
	// SGID 是玩家二维码。
	SGID string `json:"sgid"`

	// Limit 限制返回的条目数，省略时返回完整的 15 + 35 条。
	Limit int `json:"limit"`
}

// B50Result 是 b50 操作的返回结构。
type B50Result struct {
	UserID       int        `json:"userId"`
	Version      string     `json:"version"`
	Rating       int        `json:"rating"`
	NewCount     int        `json:"newCount"`
	OldCount     int        `json:"oldCount"`
	UnratedCount int        `json:"unratedCount"`
	New          []B50Entry `json:"new"`
	Old          []B50Entry `json:"old"`
}

// B50Entry 是 b50 中的一条。
type B50Entry struct {
	MusicID     int     `json:"musicId"`
	Title       string  `json:"title"`
	Level       string  `json:"level"`
	DS          float64 `json:"ds"`
	Achievement float64 `json:"achievement"`
	RA          int     `json:"ra"`
}

// runB50 拉取成绩并计算 b50。
func runB50(ctx context.Context, deps Deps, args json.RawMessage) (any, error) {
	var parsed b50Args
	if err := decodeArgs(args, &parsed); err != nil {
		return nil, err
	}
	if parsed.Limit < 0 {
		return nil, fmt.Errorf("%w: limit 不能为负", ErrBadArguments)
	}

	sgid, err := requireSGID(args)
	if err != nil {
		return nil, err
	}

	session, err := deps.Service.Fetch(ctx, sgid)
	if err != nil {
		return nil, err
	}
	index, err := deps.Service.ChartIndex(ctx)
	if err != nil {
		return nil, err
	}

	result := rating.Calc(session.Scores, index)

	unrated := 0
	for _, score := range session.Scores {
		if score.IsUtage() {
			continue
		}
		if _, ok := index.DS(score.MusicID, score.Level); !ok {
			unrated++
		}
	}

	return B50Result{
		UserID:       session.UserID,
		Version:      deps.Service.Version(),
		Rating:       result.Rating,
		NewCount:     len(result.New),
		OldCount:     len(result.Old),
		UnratedCount: unrated,
		New:          toB50Entries(result.New, parsed.Limit),
		Old:          toB50Entries(result.Old, parsed.Limit),
	}, nil
}

// toB50Entries 转换结果条目；limit 非正时全部返回。
func toB50Entries(entries []rating.Entry, limit int) []B50Entry {
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]B50Entry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, B50Entry{
			MusicID:     entry.Score.MusicID,
			Title:       entry.Title,
			Level:       entry.Score.Level.String(),
			DS:          entry.DS,
			Achievement: entry.Score.Achievement,
			RA:          entry.RA,
		})
	}
	return out
}

// versionResult 是 version 操作的返回结构。
type versionResult struct {
	Version string            `json:"version"`
	Ops     []Descriptor      `json:"operations"`
	Codes   int               `json:"errorCodeCount"`
	Index   map[string]string `json:"upstreams"`
}

// runVersion 返回版本与自描述信息，供客户端在发真实请求前探活。
func runVersion(_ context.Context, deps Deps, _ json.RawMessage) (any, error) {
	return versionResult{
		Version: deps.Service.Version(),
		Ops:     Describe(),
		Index: map[string]string{
			"title": "机台标题服务器",
			"aime":  "AimeDB 换账号",
			"chart": "查分器曲目库",
		},
	}, nil
}
