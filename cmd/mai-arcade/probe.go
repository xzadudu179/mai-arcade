package main

import (
	"context"

	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// versionDetection 是 --auto-version 的探测结果。
type versionDetection struct {
	Candidates []string `json:"candidates"`
	Detected   string   `json:"detected,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// probeOutput 在自检结果上附带版本探测信息。
//
// 内嵌 ProbeResult 使 JSON 里仍是平铺的 version / verdicts / exitCode，调用方不受影响。
type probeOutput struct {
	*service.ProbeResult
	Detection *versionDetection `json:"versionDetection,omitempty"`
}

// probeCandidates 与 service 层的探测顺序保持一致，仅用于展示。
func probeCandidates() []string { return []string{"1.55", "1.53"} }

// runProbe 连通性自检，无需凭证。
//
// 它不需要二维码，因此进程输入被忽略；结论照常写到 stdout，
// 退出码取自自检判定，便于在部署脚本里直接判断。
func runProbe(ctx context.Context, e *environment, args []string) error {
	fs := e.newFlagSet("probe")
	if err := e.parseFlags(fs, args); err != nil {
		return err
	}
	logger, err := e.logger()
	if err != nil {
		return err
	}
	svc, err := e.newService(logger)
	if err != nil {
		return err
	}

	// --auto-version 时先探出版本，再用探测到的版本做连通性判定。
	// 探测失败不直接终止：自检的价值恰恰在于把失败原因说清楚。
	var detection *versionDetection
	if e.flags.autoVersion {
		detection = &versionDetection{Candidates: probeCandidates()}
		detected, derr := svc.DetectVersion(ctx)
		if derr != nil {
			detection.Error = derr.Error()
		} else {
			detection.Detected = detected
		}
	}

	result, err := svc.Probe(ctx)
	if err != nil {
		return err
	}
	if err := emitData(e, "probe", probeOutput{ProbeResult: &result, Detection: detection}); err != nil {
		return err
	}

	// 自检未通过不是命令失败，而是结论为「有故障」：结果已写出，退出码如实反映类别。
	e.exitCode = result.ExitCode
	return nil
}

// 让 protocol 的版本表在编译期被引用：探测候选必须是真实存在的版本。
var _ = protocol.DefaultVersion
