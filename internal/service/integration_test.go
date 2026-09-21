//go:build integration

// 本文件里的测试会访问真实网络，因此单独用 integration 标签隔离：
//
//	go test -tags integration ./internal/service/ -v
//
// 涉及写入的验证（往真实查分器上传成绩）刻意不在这里自动化：
// 它会改变真实账号的数据，必须由人明确发起。手动流程见 README 的「同步验证」一节。
package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// newIntegrationService 构造使用真实网络配置的 Service。
func newIntegrationService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(Options{
		Version:  os.Getenv("MAI_VERSION"),
		ProxyURL: os.Getenv("MAI_PROXY"),
		Timeout:  60 * time.Second,
	})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	return svc
}

// TestIntegrationProbe 对真实机台与 AimeDB 做自检。
//
// 结论取决于当前出口 IP：被阻断时判为 blocked（退出码 2），这是本机最常见的状态。
// 它不断言「必须成功」，只断言判定自洽——这正是 §5.6 要求的四类故障区分能力。
func TestIntegrationProbe(t *testing.T) {
	svc := newIntegrationService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	result, err := svc.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe 报错: %v", err)
	}

	classes := map[string]FaultClass{}
	for _, v := range result.Verdicts {
		classes[v.Target] = v.Class
		t.Logf("目标 %s: class=%s ok=%v detail=%s", v.Target, v.Class, v.OK, v.Detail)
	}

	if got := classes["aime"]; got != ClassOK {
		t.Errorf("AimeDB 判定 = %q，期望 ok（该接口不受出口 IP 阻断影响）", got)
	}
	if got := classes["title"]; got == "" {
		t.Error("机台目标应当有判定结果")
	}

	// 退出码必须与判定一致。
	wantExit := combine(classes["title"], classes["aime"]).exitCode()
	if result.ExitCode != wantExit {
		t.Errorf("综合退出码 = %d, 期望 %d", result.ExitCode, wantExit)
	}
}

// TestIntegrationChartIndexFromLiveServer 用真实曲目数据校验索引与字段语义。
//
// 这条测试能发现查分器侧的结构漂移：字段改名、is_new 语义变化都会让它失败。
func TestIntegrationChartIndexFromLiveServer(t *testing.T) {
	svc := newIntegrationService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	index, err := svc.ChartIndex(ctx)
	if err != nil {
		t.Fatalf("拉取曲目索引失败: %v", err)
	}
	if index.Len() < 1000 {
		t.Fatalf("曲目数 = %d，明显偏少，可能接口结构变了", index.Len())
	}
	t.Logf("曲目数: %d", index.Len())

	// 全曲库必须同时包含 SD 与 DX 谱，且 id 命名空间与机台 musicId 一致。
	var sd, dx, utage, newSongs int
	for _, id := range []int{8, 17, 10030} {
		if _, ok := index.Song(id); ok {
			sd++
		}
	}
	for id := 10000; id < 100000; id += 997 {
		if _, ok := index.Song(id); ok {
			dx++
		}
	}
	for id := 100000; id < 200000; id += 997 {
		if _, ok := index.Song(id); ok {
			utage++
		}
	}
	if sd == 0 {
		t.Error("低 id 段（SD 谱）里一首都没找到，id 命名空间可能变了")
	}

	// is_new 必须能划分出新曲，否则 b50 的新曲 15 就无从计算。
	for _, id := range []int{8, 17, 10030, 10031, 10032, 10033} {
		if index.IsNew(id) {
			newSongs++
		}
	}
	t.Logf("抽样命中: SD=%d DX=%d 宴谱=%d is_new=%d", sd, dx, utage, newSongs)

	// 定数必须落在合理区间，抽到 0 说明 ds 字段位置变了。
	for _, id := range []int{8, 17, 10030} {
		for level := model.LevelBasic; level <= model.LevelReMaster; level++ {
			ds, ok := index.DS(id, level)
			if !ok {
				continue
			}
			if ds < 1 || ds > 16 {
				t.Errorf("musicId=%d level=%s 定数 = %v，超出合理区间，ds 字段可能错位", id, level, ds)
			}
		}
	}
}

// TestIntegrationVerifyWithRealSGID 走完整链路拉取全量成绩，验证字段含 comboStatus / syncStatus。
//
// 需要一枚有效二维码（约 10 分钟有效期），通过环境变量传入：
//
//	MAI_SGID='SGWCMAID...' go test -tags integration ./internal/service/ -run TestIntegrationVerify
func TestIntegrationVerifyWithRealSGID(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("MAI_SGID"))
	if raw == "" {
		t.Skip("未设置 MAI_SGID，跳过需要真实二维码的验证")
	}

	sgid, err := protocol.NewSGID(raw)
	if err != nil {
		t.Fatalf("MAI_SGID 不合法: %v", err)
	}
	if sgid.Expired(time.Now()) {
		t.Fatal("MAI_SGID 已过期，请重新获取（有效期约 10 分钟）")
	}

	svc := newIntegrationService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	result, err := svc.Verify(ctx, sgid)
	if err != nil {
		t.Fatalf("Verify 报错: %v", err)
	}

	if result.ScoreCount == 0 {
		t.Fatal("成绩条数为 0，该账号可能没有游玩记录，或接口返回结构有变")
	}
	if result.WithSync == 0 {
		t.Error("没有任何 syncStatus 非零的成绩，机台可能返回的是对手成绩接口而非完整成绩接口")
	}
	t.Logf("userId=%d 成绩 %d 条，带 FC 标记 %d 条，带同步标记 %d 条，宴谱 %d 条",
		result.UserID, result.ScoreCount, result.WithCombo, result.WithSync, result.UtageCount)
	t.Logf("comboStatus 分布: %v", result.ComboCounts)
	t.Logf("syncStatus 分布: %v", result.SyncCounts)
}

// TestIntegrationDefaultProtocolVersionIsAccepted 断言默认协议版本仍被服务端接受。
//
// 这条测试防的是一种很难查的故障：服务端对「不再接受的协议版本」返回的是
// HTTP 200 + 0 字节，与出口 IP 被阻断的症状完全一样。默认版本一旦过期，
// 所有使用者都会看到一个假的「IP 被阻断」。游戏更新后这条测试会立刻变红。
func TestIntegrationDefaultProtocolVersionIsAccepted(t *testing.T) {
	hc, err := transport.New(transport.Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("构造传输客户端失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 先确认链路本身可用，否则「版本全都不行」的结论没有意义。
	if err := hc.HostReachable(ctx, protocol.DefaultTitleBaseURL); err != nil {
		t.Skipf("机台不可达，跳过版本验证: %v", err)
	}

	accepted := make([]string, 0, 4)
	walk := append([]string{protocol.DefaultVersion}, "1.55", "1.53", "1.52", "1.50")
	for _, name := range walk {
		version, err := protocol.LookupVersion(name)
		if err != nil {
			continue
		}
		client, err := protocol.NewTitleClient(protocol.TitleOptions{HTTP: hc, Version: version})
		if err != nil {
			continue
		}

		pingErr := client.Ping(ctx)
		switch {
		case pingErr == nil:
			t.Logf("版本 %-5s -> 正常", name)
			accepted = append(accepted, name)
		case errors.Is(pingErr, protocol.ErrEmptyResponse):
			t.Logf("版本 %-5s -> 0 字节（该版本已不被接受）", name)
		default:
			t.Logf("版本 %-5s -> %v", name, pingErr)
		}
	}

	if len(accepted) == 0 {
		t.Skip("没有任何版本拿到可用响应：可能是出口 IP 被阻断，换环境再验证")
	}

	// 默认版本必须在可用列表里，否则所有使用者都会被误导。
	for _, name := range accepted {
		if name == protocol.DefaultVersion {
			t.Logf("默认版本 %s 可用；本次实测可用版本: %v", protocol.DefaultVersion, accepted)
			return
		}
	}
	t.Errorf("默认版本 %s 已不被服务端接受，可用版本为 %v：请更新 protocol.DefaultVersion",
		protocol.DefaultVersion, accepted)
}
