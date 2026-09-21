package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// versionProbeOrder 是多版本自动探测的顺序：新版本优先。
//
// 只探测 1.53 及以后：更早的版本压缩与加密顺序相反（见 protocol.Version.ZlibBeforeEncrypt），
// 它们对应的机台早就升级了，把它们排进探测序列只会平白多打几次上游。
var versionProbeOrder = []string{"1.55", "1.53"}

// DetectVersion 依次尝试候选协议版本，返回第一个能解开机台响应的版本。
//
// 探测依据是「能不能解开响应」而不是「HTTP 是否成功」：出口 IP 被阻断或网络不通时
// 任何版本都拿不到可用响应，此时立即中止并把原因原样带回，不做无意义的逐个尝试。
func (s *Service) DetectVersion(ctx context.Context) (string, error) {
	if s.versionProbeDisabled {
		return s.Version(), nil
	}

	titleBase := s.opts.TitleBaseURL
	if titleBase == "" {
		titleBase = protocol.DefaultTitleBaseURL
	}

	// 先确认链路本身可用，避免把「被阻断」误判成「版本不对」。
	if err := s.hc.HostReachable(ctx, titleBase); err != nil {
		return "", fmt.Errorf("%w: %v", protocol.ErrNetwork, err)
	}

	var lastErr error
	for _, name := range versionProbeOrder {
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
			continue
		}

		err = client.Ping(ctx)
		switch {
		case err == nil:
			s.useVersion(version)
			if s.logger != nil {
				s.logger.Info("协议版本探测成功", "版本", version.Encoding)
			}
			return version.Encoding, nil

		case isVersionMismatch(err):
			// 这个版本解不开：继续试下一个。
			lastErr = err
			if s.logger != nil {
				s.logger.Debug("协议版本不可用，继续尝试", "版本", version.Encoding)
			}
			continue

		default:
			// 网络、阻断或业务原因：换版本也解决不了，直接返回。
			return "", err
		}
	}
	return "", fmt.Errorf("%w：试过 %s", lastErr, strings.Join(versionProbeOrder, "、"))
}

// isVersionMismatch 报告错误是否属于「参数版本不对」。
func isVersionMismatch(err error) bool {
	return err != nil && (errors.Is(err, protocol.ErrDecrypt) || errors.Is(err, protocol.ErrVersionMismatch))
}

// useVersion 记录探测到的可用版本，后续请求都用它。
func (s *Service) useVersion(version protocol.Version) {
	s.versionMu.Lock()
	defer s.versionMu.Unlock()
	s.version = version
	s.detected = true
}

// VersionDetected 报告当前版本是探测得来还是配置指定。
func (s *Service) VersionDetected() bool {
	s.versionMu.RLock()
	defer s.versionMu.RUnlock()
	return s.detected
}
