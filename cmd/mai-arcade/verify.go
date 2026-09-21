package main

import (
	"context"

	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// verifyOutput 是 verify 的输出结构：只描述字段结构，不含凭证。
type verifyOutput struct {
	UserID        int    `json:"userId"`
	Version       string `json:"version"`
	ScoreCount    int    `json:"scoreCount"`
	WithCombo     int    `json:"withComboStatus"`
	WithSync      int    `json:"withSyncStatus"`
	UtageCount    int    `json:"utageCount"`
	UtageUnmapped int    `json:"utageUnmapped,omitempty"`
	ComboCounts   any    `json:"comboStatusCounts"`
	SyncCounts    any    `json:"syncStatusCounts"`
	LevelCounts   any    `json:"levelCounts"`
	Utages        any    `json:"utages,omitempty"`
	Samples       any    `json:"samples"`
	Warnings      any    `json:"warnings,omitempty"`
	Note          string `json:"note"`
}

// runVerify 读 stdin 的二维码，走完整链路拉取全量成绩并打印字段结构，不上传任何数据。
//
// 它的验收意义在于：只要输出里 withComboStatus / withSyncStatus 不为 0，
// 就说明机台完整成绩接口（而非对手成绩接口）确实带着 FC/FS 回来了。
func runVerify(ctx context.Context, e *environment, args []string) error {
	fs := e.newFlagSet("verify")
	if err := e.parseFlags(fs, args); err != nil {
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

	result, err := svc.Verify(ctx, sgid)
	if err != nil {
		return err
	}
	return emitData(e, "verify", verifyOutput{
		UserID:        result.UserID,
		Version:       svc.Version(),
		ScoreCount:    result.ScoreCount,
		WithCombo:     result.WithCombo,
		WithSync:      result.WithSync,
		UtageCount:    result.UtageCount,
		UtageUnmapped: result.UtageUnmapped,
		ComboCounts:   result.ComboCounts,
		SyncCounts:    result.SyncCounts,
		LevelCounts:   result.LevelCounts,
		Utages:        utagesOrNil(result.Utages),
		Samples:       result.Samples,
		Warnings:      warningsOrNil(result.Warnings),
		Note: "未上传任何数据；成绩条数 >0 且 withSyncStatus >0 说明机台返回的是含竞速标识的完整成绩。" +
			"utages 列出宴谱的映射结果：title 非空即表示能同步，levelIndex 是折算后的难度（宴谱恒为 0）",
	})
}

// utagesOrNil 让没有宴谱时不出现在 JSON 里。
func utagesOrNil(samples []service.UtageSample) any {
	if len(samples) == 0 {
		return nil
	}
	return samples
}

// warningsOrNil 让 warnings 为空时不出现在 JSON 里。
func warningsOrNil(w []string) any {
	if len(w) == 0 {
		return nil
	}
	return w
}
