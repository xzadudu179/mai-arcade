package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/chart"
	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// isKind 判定错误分类。sync 不依赖 protocol，因此直接用 model 里共享的分类判定。
func isKind(err error, k model.Kind) bool {
	var me *model.Error
	return errors.As(err, &me) && me.Kind == k
}

// newTestHTTP 构造测试用传输客户端。
func newTestHTTP(t *testing.T) *transport.Client {
	t.Helper()
	hc, err := transport.New(transport.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造传输客户端失败: %v", err)
	}
	return hc
}

// fakeDivingFish 模拟水鱼查分器的两个端点。
type fakeDivingFish struct {
	t *testing.T

	// recordsStatus / recordsBody 控制 GET /player/records 的响应。
	recordsStatus int
	recordsBody   string

	// uploadStatus 控制 POST /player/update_records 的响应。
	uploadStatus int

	uploadHeaders http.Header
	uploadBody    []byte

	loadCalls   atomic.Int64
	uploadCalls atomic.Int64

	server *httptest.Server
}

// newFakeDivingFish 启动假查分器。
func newFakeDivingFish(t *testing.T, recordsStatus int, recordsBody string, uploadStatus int) *fakeDivingFish {
	t.Helper()
	f := &fakeDivingFish{t: t, recordsStatus: recordsStatus, recordsBody: recordsBody, uploadStatus: uploadStatus}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeDivingFish) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case divingFishRecordsPath:
		f.loadCalls.Add(1)
		w.WriteHeader(f.recordsStatus)
		if f.recordsBody != "" {
			_, _ = w.Write([]byte(f.recordsBody))
		}
	case divingFishUpdatePath:
		f.uploadCalls.Add(1)
		f.uploadHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("读取上传体失败: %v", err)
		}
		f.uploadBody = body
		w.WriteHeader(f.uploadStatus)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	default:
		f.t.Errorf("意外的请求路径 %q", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// newSyncer 构造指向假查分器的水鱼实现。
func (f *fakeDivingFish) newSyncer(songs chart.Source) *DivingFish {
	return NewDivingFish(Deps{
		HTTP:    newTestHTTP(f.t),
		Songs:   songs,
		BaseURL: f.server.URL,
	})
}

// testScores 是机台上报的样例成绩。
func testScores() []model.Score {
	return []model.Score{
		{
			MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5, DXScore: 2711,
			PlayCount: 3,
		},
	}
}

// TestDivingFishUploadPreservesRemoteMarks 断言真实上传路径上 FC/FS 被保留。
//
// 这条覆盖「读现状 → 合并 → 上传」的完整链路，而不只是合并函数本身。
func TestDivingFishUploadPreservesRemoteMarks(t *testing.T) {
	server := newFakeDivingFish(t, http.StatusOK, `{"records":[
		{"song_id":1001,"title":"Alea jacta est!","type":"DX","level_index":3,
		 "achievements":100.1234,"dxScore":2600,"fc":"ap","fs":"fsd"}]}`, http.StatusOK)

	syncer := server.newSyncer(testIndex())
	if err := syncer.Upload(context.Background(), testScores(), "import-token-abc"); err != nil {
		t.Fatalf("上传失败: %v", err)
	}

	if server.loadCalls.Load() != 1 || server.uploadCalls.Load() != 1 {
		t.Fatalf("读取/上传次数 = %d/%d, 期望 1/1", server.loadCalls.Load(), server.uploadCalls.Load())
	}

	// 两个请求都必须带 Import-Token。
	if got := server.uploadHeaders.Get(divingFishCredentialHeader); got != "import-token-abc" {
		t.Errorf("上传请求缺少 Import-Token: %q", got)
	}

	var uploaded []uploadRecord
	if err := json.Unmarshal(server.uploadBody, &uploaded); err != nil {
		t.Fatalf("上传体不是合法 JSON 数组: %v（原文 %s）", err, server.uploadBody)
	}
	if len(uploaded) != 1 {
		t.Fatalf("上传条数 = %d, 期望 1", len(uploaded))
	}

	got := uploaded[0]
	if got.FC != "ap" {
		t.Errorf("fc = %q, 期望保留服务器原值 \"ap\"", got.FC)
	}
	if got.FS != "fsd" {
		t.Errorf("fs = %q, 期望保留服务器原值 \"fsd\"", got.FS)
	}
	if got.Title != "Alea jacta est!" || got.Type != "DX" {
		t.Errorf("曲目定位字段不符: title=%q type=%q", got.Title, got.Type)
	}
	if got.LevelIndex != int(model.LevelMaster) {
		t.Errorf("level_index = %d, 期望 %d", got.LevelIndex, model.LevelMaster)
	}
	if got.Achievements != 100.5 || got.DXScore != 2711 {
		t.Errorf("成绩字段不符: achievements=%v dxScore=%d", got.Achievements, got.DXScore)
	}
}

// TestDivingFishUploadSendsBareArray 断言请求体是裸 JSON 数组而不是对象包装。
//
// 包成 {"records": [...]} 会被服务端拒绝，这是最容易写错的一处。
func TestDivingFishUploadSendsBareArray(t *testing.T) {
	server := newFakeDivingFish(t, http.StatusOK, `{"records":[]}`, http.StatusOK)

	syncer := server.newSyncer(testIndex())
	if err := syncer.Upload(context.Background(), testScores(), "token"); err != nil {
		t.Fatalf("上传失败: %v", err)
	}

	trimmed := strings.TrimSpace(string(server.uploadBody))
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		t.Errorf("上传体应当是裸 JSON 数组，实际: %s", trimmed)
	}

	var asMap map[string]any
	if err := json.Unmarshal(server.uploadBody, &asMap); err == nil {
		t.Error("上传体被解析成了对象，说明包了一层")
	}
}

// TestDivingFishAbortsWhenRemoteFetchFails 断言读不到现状时绝不上传。
//
// 这是最能造成破坏的路径：把「读失败」当成「服务器没有成绩」，
// 接着上传就会把已有的 FC/FS 全部清空。
func TestDivingFishAbortsWhenRemoteFetchFails(t *testing.T) {
	tests := []struct {
		name          string
		recordsStatus int
		recordsBody   string
	}{
		{"响应 500", http.StatusInternalServerError, `{"status":"error"}`},
		{"响应为空", http.StatusOK, ""},
		{"响应为 null", http.StatusOK, "null"},
		{"响应是拦截页", http.StatusOK, "<html>维护中</html>"},
		{"响应结构不对", http.StatusOK, `{"records":"不是数组"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeDivingFish(t, tc.recordsStatus, tc.recordsBody, http.StatusOK)

			syncer := server.newSyncer(testIndex())
			err := syncer.Upload(context.Background(), testScores(), "token")
			if err == nil {
				t.Fatal("读现状失败时应当报错")
			}
			if server.uploadCalls.Load() != 0 {
				t.Errorf("读现状失败时不应上传，实际上传 %d 次", server.uploadCalls.Load())
			}
		})
	}
}

// TestDivingFishRejectsEmptyCredential 断言没有凭证时在本地就拦下，不发任何请求。
func TestDivingFishRejectsEmptyCredential(t *testing.T) {
	server := newFakeDivingFish(t, http.StatusOK, `{"records":[]}`, http.StatusOK)
	syncer := server.newSyncer(testIndex())

	for _, token := range []string{"", "   "} {
		err := syncer.Upload(context.Background(), testScores(), token)
		if !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("token=%q 的错误 = %v, 期望 %v", token, err, ErrInvalidCredential)
		}
	}
	if server.loadCalls.Load() != 0 {
		t.Errorf("缺少凭证时不应发出请求，实际 %d 次", server.loadCalls.Load())
	}
}

// TestDivingFishMaps400ToInvalidCredential 断言 400 被翻译成「Token 无效」。
func TestDivingFishMaps400ToInvalidCredential(t *testing.T) {
	tests := []struct {
		name          string
		recordsStatus int
		uploadStatus  int
	}{
		{"读取现状时 400", http.StatusBadRequest, http.StatusOK},
		{"上传时 400", http.StatusOK, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeDivingFish(t, tc.recordsStatus, `{"records":[]}`, tc.uploadStatus)

			syncer := server.newSyncer(testIndex())
			err := syncer.Upload(context.Background(), testScores(), "bad-token")
			if !errors.Is(err, ErrInvalidCredential) {
				t.Fatalf("错误 = %v, 期望 %v", err, ErrInvalidCredential)
			}
			if !isKind(err, model.KindBusiness) {
				t.Error("凭证无效应归为业务失败（退出码 1）")
			}
			if !strings.Contains(err.Error(), "Import-Token") {
				t.Errorf("错误信息应指导用户重新生成 Import-Token: %v", err)
			}
		})
	}
}

// TestDivingFishRequiresChartIndex 断言没有曲目索引时拒绝上传。
//
// 没有索引就无法把 musicId 翻成水鱼要求的歌名，硬传会全部匹配失败。
func TestDivingFishRequiresChartIndex(t *testing.T) {
	server := newFakeDivingFish(t, http.StatusOK, `{"records":[]}`, http.StatusOK)
	syncer := NewDivingFish(Deps{HTTP: newTestHTTP(t), Songs: nil, BaseURL: server.server.URL})

	err := syncer.Upload(context.Background(), testScores(), "token")
	if !errors.Is(err, ErrNoSongs) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrNoSongs)
	}
	if server.loadCalls.Load() != 0 {
		t.Error("缺少索引时不应发出请求")
	}
}

// TestDivingFishRejectsUnmappableScores 断言一条都映射不出来时放弃上传。
func TestDivingFishRejectsUnmappableScores(t *testing.T) {
	server := newFakeDivingFish(t, http.StatusOK, `{"records":[]}`, http.StatusOK)
	// 索引非 nil 但为空：等价于曲目数据拉取失败或过期。
	syncer := server.newSyncer(chart.NewIndex(nil))

	err := syncer.Upload(context.Background(), testScores(), "token")
	if !errors.Is(err, ErrNoMappableScores) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrNoMappableScores)
	}
	if server.uploadCalls.Load() != 0 {
		t.Errorf("映射失败时不应上传，实际 %d 次", server.uploadCalls.Load())
	}
}

// TestDivingFishUploadsEmptyListWhenMachineHasNoScores 断言机台确实没有成绩时不报错。
//
// 与上一条的区别在于「机台本来就没打过」是合法状态，不该当成失败。
func TestDivingFishUploadsEmptyListWhenMachineHasNoScores(t *testing.T) {
	server := newFakeDivingFish(t, http.StatusOK, `{"records":[]}`, http.StatusOK)
	syncer := server.newSyncer(testIndex())

	if err := syncer.Upload(context.Background(), nil, "token"); err != nil {
		t.Fatalf("空成绩列表不应报错: %v", err)
	}
	if server.uploadCalls.Load() != 1 {
		t.Errorf("上传次数 = %d, 期望 1", server.uploadCalls.Load())
	}
}

// TestDivingFishName 断言注册名与 CLI 取值一致。
func TestDivingFishName(t *testing.T) {
	syncer := NewDivingFish(Deps{})
	if got := syncer.Name(); got != divingFishName {
		t.Errorf("Name() = %q, 期望 %q", got, divingFishName)
	}
}

// TestRegistryLookup 断言注册表按名取用并给出可用取值。
func TestRegistryLookup(t *testing.T) {
	if _, err := New(divingFishName, Deps{HTTP: newTestHTTP(t)}); err != nil {
		t.Fatalf("按名取水鱼实现失败: %v", err)
	}

	_, err := New("nope", Deps{})
	if err == nil {
		t.Fatal("未知查分器应当报错")
	}
	if !isKind(err, model.KindParam) {
		t.Errorf("未知查分器应归为参数错误（退出码 3），得到 %v", err)
	}
	if !strings.Contains(err.Error(), divingFishName) {
		t.Errorf("错误信息应列出可用取值: %v", err)
	}

	found := false
	for _, name := range Names() {
		if name == divingFishName {
			found = true
		}
	}
	if !found {
		t.Errorf("Names() 应包含 %q，实际 %v", divingFishName, Names())
	}
}

// TestRegisterRejectsDuplicate 断言重名注册会立刻 panic，而不是被静默覆盖。
func TestRegisterRejectsDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("重名注册应当 panic")
		}
	}()
	Register(divingFishName, func(Deps) Syncer { return nil })
}

// TestNamesIsSorted 断言 Names 顺序稳定，便于测试与提示文案。
func TestNamesIsSorted(t *testing.T) {
	names := Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Names() 未排序: %v", names)
		}
	}
}
