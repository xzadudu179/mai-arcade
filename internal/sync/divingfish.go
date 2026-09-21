package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// 水鱼相关端点路径。
const (
	divingFishRecordsPath = "/player/records"
	divingFishUpdatePath  = "/player/update_records"
)

// 水鱼层具名哨兵。
var (
	// ErrInvalidCredential 表示 Import-Token 无效或为空。
	ErrInvalidCredential = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeSiteCredential,
		Msg:  "水鱼 Import-Token 无效",
		Hint: "登录水鱼查分器官网，在「编辑个人资料」中重新生成 Import-Token",
	}

	// ErrUnexpectedStatus 表示查分器返回了非预期状态码。
	ErrUnexpectedStatus = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeSiteStatus,
		Msg:  "水鱼查分器返回非预期状态",
		Hint: "稍后重试；若持续出现，确认服务状态与所选查分器地址",
	}

	// ErrNoSongs 表示没有曲目索引，无法把 musicId 翻译成歌名。
	ErrNoSongs = &model.Error{
		Kind: model.KindParam, Sentinel: model.CodeSiteNoSongs,
		Msg:  "缺少曲目索引，无法确定歌名",
		Hint: "确认能访问查分器的 music_data 接口",
	}

	// ErrUploadFailed 表示上传成绩时服务端返回非预期状态。
	//
	// 与读取阶段的失败分开：读取失败可以原样重试，上传失败则要先确认服务端是否
	// 已经写入了一部分，盲目重试可能造成重复提交。
	ErrUploadFailed = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeSiteUploadFail,
		Msg:  "查分器拒绝接收成绩",
		Hint: "先回查分器网页确认是否已部分写入，再决定重试；持续失败时检查导入 Token 与服务状态",
	}

	// ErrRecordsUnparsable 表示读取现状的响应无法解析。
	//
	// 这条必须是硬失败：把「读失败」当成「服务器没有成绩」，紧接着的上传就会清空已有标记。
	ErrRecordsUnparsable = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeSiteRecordsBad,
		Msg:  "查分器成绩响应无法解析",
		Hint: "确认 Import-Token 有效、服务地址正确；不要在上传前忽略此错误",
	}

	// ErrNoMappableScores 表示机台成绩一条都没能映射到曲目。
	ErrNoMappableScores = &model.Error{
		Kind: model.KindBusiness, Sentinel: model.CodeSiteUnmappable,
		Msg:  "机台成绩无法映射到任何曲目，已放弃上传",
		Hint: "曲目索引可能过期，稍后重试；若持续失败请保留日志排查",
	}
)

// init 注册水鱼实现：新增查分器就照这个形状写一行。
func init() {
	Register(divingFishName, func(deps Deps) Syncer {
		return NewDivingFish(deps)
	})
}

// DivingFish 是水鱼查分器的同步实现。
type DivingFish struct {
	hc      *transport.Client
	songs   chart.Source
	baseURL string
	logger  *slog.Logger
}

// NewDivingFish 构造水鱼实现。
func NewDivingFish(deps Deps) *DivingFish {
	base := deps.BaseURL
	if base == "" {
		base = chart.DefaultBaseURL
	}
	return &DivingFish{
		hc:      deps.HTTP,
		songs:   deps.Songs,
		baseURL: strings.TrimSuffix(base, "/"),
		logger:  deps.Logger,
	}
}

// Name 返回注册名。
func (d *DivingFish) Name() string { return divingFishName }

// Upload 先读服务器现状、再合并、最后上传。
//
// 顺序不可颠倒：fc/fs 传空会清空服务器已有标记，所以必须先拿到现状才能保住它们。
func (d *DivingFish) Upload(ctx context.Context, scores []model.Score, credential string) error {
	if strings.TrimSpace(credential) == "" {
		return ErrInvalidCredential
	}
	if d.songs == nil {
		return ErrNoSongs
	}

	remote, err := d.loadRemote(ctx, credential)
	if err != nil {
		return err
	}

	merged := merge(remote, scores, d.songs)
	if d.logger != nil {
		d.logger.Info("合并完成",
			"机台条数", len(scores),
			"服务器条数", len(remote),
			"待上传条数", len(merged.Records),
			"保留标记数", merged.PreservedMarks,
			"其中宴谱", merged.Utage,
			"未知曲目跳过", merged.SkippedUnknown,
			"越界难度跳过", merged.SkippedInvalidLevel,
			"服务器独有", merged.RemoteOnly)
	}
	// 一条都映射不出来，几乎总是曲目索引为空或过期，此时上传空数组毫无意义。
	if len(merged.Records) == 0 && len(scores) > 0 {
		return ErrNoMappableScores
	}
	return d.upload(ctx, merged.Records, credential)
}

// loadRemote 读取服务器当前成绩，作为合并的基准。
func (d *DivingFish) loadRemote(ctx context.Context, credential string) ([]remoteRecord, error) {
	headers := map[string]string{divingFishCredentialHeader: credential}
	resp, err := d.hc.Get(ctx, d.baseURL+divingFishRecordsPath, headers)
	if err != nil {
		return nil, fmt.Errorf("读取水鱼现有成绩: %w", err)
	}
	if err := divingFishStatusError(stageRead, resp.StatusCode); err != nil {
		return nil, err
	}

	records, err := parseRecordsBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w（HTTP %d）: %v", ErrRecordsUnparsable, resp.StatusCode, err)
	}
	return records, nil
}

// upload 把合并结果作为裸 JSON 数组发出。
func (d *DivingFish) upload(ctx context.Context, records []uploadRecord, credential string) error {
	body, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("序列化上传内容: %w", err)
	}

	headers := map[string]string{
		divingFishCredentialHeader: credential,
		"Content-Type":             "application/json",
	}
	resp, err := d.hc.Post(ctx, d.baseURL+divingFishUpdatePath, headers, body)
	if err != nil {
		return fmt.Errorf("上传水鱼成绩: %w", err)
	}
	if err := divingFishStatusError(stageUpload, resp.StatusCode); err != nil {
		return err
	}
	return nil
}

// requestStage 区分同一次同步里的两个 HTTP 阶段。
type requestStage int

const (
	stageRead   requestStage = iota // 读服务器现状
	stageUpload                     // 上传成绩
)

// divingFishStatusError 把状态码翻译成带补救建议的错误。
//
// 400 一律是凭证问题；其余非 200 按阶段区分——读失败可以原样重试，
// 上传失败则要先确认服务端是否已写入一部分，两者的处置不同。
func divingFishStatusError(stage requestStage, status int) error {
	if status == http.StatusOK {
		return nil
	}
	if status == http.StatusBadRequest {
		return ErrInvalidCredential
	}

	sentinel := ErrUnexpectedStatus
	if stage == stageUpload {
		sentinel = ErrUploadFailed
	}
	return fmt.Errorf("%w（HTTP %d）", sentinel, status)
}

// sentinels 是查分器层全部具名错误码，集中登记。
var sentinels = []*model.Error{
	ErrInvalidCredential,
	ErrUnexpectedStatus,
	ErrNoSongs,
	ErrNoMappableScores,
	ErrRecordsUnparsable,
	ErrUploadFailed,
}

func init() {
	for _, sentinel := range sentinels {
		model.RegisterCode(sentinel)
	}
}
