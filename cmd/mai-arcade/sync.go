package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/xzadudu179/maimai-arcade/internal/service"
	sitesync "github.com/xzadudu179/maimai-arcade/internal/sync"
)

// syncOutput 是 sync 的输出结构。凭证与二维码都不出现在这里。
type syncOutput struct {
	UserID     int    `json:"userId"`
	Site       string `json:"site"`
	ScoreCount int    `json:"scoreCount"`
	UtageCount int    `json:"utageCount"`
	Warnings   any    `json:"warnings,omitempty"`
	Note       string `json:"note"`
}

// runSync 读 stdin 的二维码，拉取全量成绩并同步到指定查分器。
//
// 同步前会先读查分器现状并合并：fc/fs 传空会清空服务器已有标记，
// 而机台在部分情况下拿不到这两项，因此合并是必需的而不是优化。
func runSync(ctx context.Context, e *environment, args []string) error {
	fs := e.newFlagSet("sync")
	var (
		site       = fs.String("site", "divingfish", "目标查分器，可用："+strings.Join(sitesync.Names(), "/"))
		credential = fs.String("credential", "", "查分器凭证（水鱼为 Import-Token）")
		fishToken  = fs.String("fish-token", "", "水鱼 Import-Token 的简写")
	)
	if err := e.parseFlags(fs, args); err != nil {
		return err
	}

	token, err := resolveCredential(*site, *credential, *fishToken)
	if err != nil {
		return err
	}
	logger, err := e.logger()
	if err != nil {
		return err
	}
	sgid, err := e.readSGID()
	if err != nil {
		return err
	}
	svc, err := e.prepare(ctx, logger)
	if err != nil {
		return err
	}

	result, err := svc.Sync(ctx, service.SyncRequest{
		SGID:       sgid,
		Site:       *site,
		Credential: token,
	})
	if err != nil {
		return err
	}
	return emitData(e, "sync", syncOutput{
		UserID:     result.UserID,
		Site:       result.Site,
		ScoreCount: result.ScoreCount,
		UtageCount: result.UtageCount,
		Warnings:   warningsOrNil(result.Warnings),
		Note:       "已合并服务器现状后上传，机台缺失的 FC/FS 保留原值；宴谱一并上传但不计入 b50",
	})
}

// resolveCredential 归一化凭证参数：--credential 与 --fish-token 不能同时给出，也不能都不给。
func resolveCredential(site, credential, fishToken string) (string, error) {
	switch {
	case credential != "" && fishToken != "":
		return "", fmt.Errorf("%w: --credential 与 --fish-token 只能给一个", usageError)
	case fishToken != "":
		return fishToken, nil
	case credential != "":
		return credential, nil
	default:
		return "", fmt.Errorf("%w: --site %s 需要 --credential（水鱼可写 --fish-token）", usageError, site)
	}
}
