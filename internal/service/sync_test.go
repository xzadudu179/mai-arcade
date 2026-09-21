package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/protocol"
)

// fakeDivingFishServer 同时扮演水鱼的三个端点：曲目数据、读取现状、上传成绩。
type fakeDivingFishServer struct {
	t *testing.T

	mu            sync.Mutex
	musicData     string
	recordsBody   string
	recordsStatus int
	uploadBody    []byte
	uploadCalls   int
	recordsCalls  int
}

// newFakeDivingFishServer 启动假水鱼。
func newFakeDivingFishServer(t *testing.T, musicData, recordsBody string, recordsStatus int) (*fakeDivingFishServer, string) {
	t.Helper()
	f := &fakeDivingFishServer{
		t:             t,
		musicData:     musicData,
		recordsBody:   recordsBody,
		recordsStatus: recordsStatus,
	}
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(server.Close)
	return f, server.URL
}

func (f *fakeDivingFishServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/music_data":
		_, _ = w.Write([]byte(f.musicData))
	case "/player/records":
		f.recordsCalls++
		w.WriteHeader(f.recordsStatus)
		if f.recordsBody != "" {
			_, _ = w.Write([]byte(f.recordsBody))
		}
	case "/player/update_records":
		f.uploadCalls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("读取上传体失败: %v", err)
		}
		f.uploadBody = body
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	default:
		f.t.Errorf("意外的路径 %q", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// uploads 返回上传次数。
func (f *fakeDivingFishServer) uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploadCalls
}

// testMusicData 是一份最小可用的曲目数据。
const testMusicData = `[
 {"id":"1001","title":"Alea jacta est!","type":"DX","ds":[3.0,7.0,10.0,13.0],
  "basic_info":{"is_new":false}},
 {"id":"1002","title":"新曲样本","type":"DX","ds":[4.0,8.0,11.0,14.0],
  "basic_info":{"is_new":true}}
]`

// TestSyncUploadsMergedRecords 断言 Sync 走完「取索引 → 读现状 → 合并 → 上传」。
//
// 机台这条成绩没有竞速标识，服务器上原有 ap/fsd，上传内容必须保住它们。
func TestSyncUploadsMergedRecords(t *testing.T) {
	df, baseURL := newFakeDivingFishServer(t,
		testMusicData,
		`{"records":[{"song_id":1001,"title":"Alea jacta est!","type":"DX","level_index":3,
		 "achievements":100.1234,"dxScore":2600,"fc":"ap","fs":"fsd"}]}`,
		http.StatusOK)

	svc, err := New(Options{ChartBaseURL: baseURL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	// 直接给出成绩，跳过机台会话：这条测试关心的是同步链路。
	scores := []model.Score{
		{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.5, DXScore: 2711, PlayCount: 3},
	}

	result, err := svc.Sync(context.Background(), SyncRequest{
		Site:       "divingfish",
		Credential: "import-token-abc",
		Scores:     scores,
	})
	if err != nil {
		t.Fatalf("Sync 报错: %v", err)
	}

	if result.Site != "divingfish" {
		t.Errorf("Site = %q, 期望 divingfish", result.Site)
	}
	if result.ScoreCount != 1 {
		t.Errorf("ScoreCount = %d, 期望 1", result.ScoreCount)
	}
	if df.uploads() != 1 {
		t.Fatalf("上传次数 = %d, 期望 1", df.uploads())
	}

	var uploaded []struct {
		Title        string  `json:"title"`
		Type         string  `json:"type"`
		LevelIndex   int     `json:"level_index"`
		Achievements float64 `json:"achievements"`
		FC           string  `json:"fc"`
		FS           string  `json:"fs"`
	}
	if err := json.Unmarshal(df.uploadBody, &uploaded); err != nil {
		t.Fatalf("上传体解析失败: %v（原文 %s）", err, df.uploadBody)
	}
	if len(uploaded) != 1 {
		t.Fatalf("上传条数 = %d, 期望 1", len(uploaded))
	}

	got := uploaded[0]
	if got.FC != "ap" || got.FS != "fsd" {
		t.Errorf("FC/FS 未被保留: fc=%q fs=%q", got.FC, got.FS)
	}
	// 歌名必须来自曲目索引：机台只给 musicId，水鱼靠歌名匹配曲目。
	if got.Title != "Alea jacta est!" || got.Type != "DX" {
		t.Errorf("曲目定位字段不符: title=%q type=%q", got.Title, got.Type)
	}
	if got.Achievements != 100.5 {
		t.Errorf("achievements = %v, 期望 100.5", got.Achievements)
	}
}

// TestSyncRunsFullSessionWhenScoresNotProvided 断言未预置成绩时 Sync 会自己走一遍机台会话。
func TestSyncRunsFullSessionWhenScoresNotProvided(t *testing.T) {
	df, baseURL := newFakeDivingFishServer(t, testMusicData, `{"records":[]}`, http.StatusOK)

	session := &fakeTitleSession{
		scores: []model.Score{
			{MusicID: 1002, Level: model.LevelMaster, Achievement: 100.5, PlayCount: 2},
		},
	}
	aime, _ := fakeAimeServer(t)

	svc, err := New(Options{ChartBaseURL: baseURL, AimeURL: aime.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	svc.newTitle = func(protocol.TitleOptions) (titleSession, error) { return session, nil }

	result, err := svc.Sync(context.Background(), SyncRequest{
		SGID:       validSGID(t),
		Site:       "divingfish",
		Credential: "token",
	})
	if err != nil {
		t.Fatalf("Sync 报错: %v", err)
	}

	if session.loginCalls.Load() != 1 || session.logoutCalls.Load() != 1 {
		t.Errorf("登录/登出次数 = %d/%d, 期望 1/1",
			session.loginCalls.Load(), session.logoutCalls.Load())
	}
	if result.UserID != 10807675 {
		t.Errorf("UserID = %d, 期望取自 AimeDB 响应", result.UserID)
	}
	if df.uploads() != 1 {
		t.Errorf("上传次数 = %d, 期望 1", df.uploads())
	}
	if !strings.Contains(string(df.uploadBody), "新曲样本") {
		t.Errorf("上传内容应包含新曲样本: %s", df.uploadBody)
	}
}

// TestSyncDefaultsToDivingFish 断言不指定站点时默认走水鱼。
func TestSyncDefaultsToDivingFish(t *testing.T) {
	_, baseURL := newFakeDivingFishServer(t, testMusicData, `{"records":[]}`, http.StatusOK)

	svc, err := New(Options{ChartBaseURL: baseURL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	result, err := svc.Sync(context.Background(), SyncRequest{
		Credential: "token",
		Scores:     []model.Score{{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.0, PlayCount: 1}},
	})
	if err != nil {
		t.Fatalf("Sync 报错: %v", err)
	}
	if result.Site != "divingfish" {
		t.Errorf("Site = %q, 期望默认 divingfish", result.Site)
	}
}

// TestSyncFailsBeforeSessionWhenChartIndexUnavailable 断言曲目索引拉不到时不占用机台会话。
//
// 少一次登录就少一次被封禁的机会，这也是把取索引放在会话之前的原因。
func TestSyncFailsBeforeSessionWhenChartIndexUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	session := &fakeTitleSession{}
	aime, aimeCalls := fakeAimeServer(t)

	svc, err := New(Options{ChartBaseURL: server.URL, AimeURL: aime.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	svc.newTitle = func(protocol.TitleOptions) (titleSession, error) { return session, nil }

	_, err = svc.Sync(context.Background(), SyncRequest{
		SGID:       validSGID(t),
		Credential: "token",
	})
	if err == nil {
		t.Fatal("曲目索引不可用时应当报错")
	}
	if session.loginCalls.Load() != 0 {
		t.Error("索引拉取失败时不应登录机台")
	}
	if aimeCalls.Load() != 0 {
		t.Error("索引拉取失败时不应调用 AimeDB")
	}
}

// TestSyncReportsUnknownSite 断言未知站点归为参数错误。
func TestSyncReportsUnknownSite(t *testing.T) {
	_, baseURL := newFakeDivingFishServer(t, testMusicData, `{"records":[]}`, http.StatusOK)

	svc, err := New(Options{ChartBaseURL: baseURL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	_, err = svc.Sync(context.Background(), SyncRequest{
		Site:       "nope",
		Credential: "token",
		Scores:     []model.Score{{MusicID: 1001, Level: model.LevelMaster, Achievement: 100.0, PlayCount: 1}},
	})
	if err == nil {
		t.Fatal("未知站点应当报错")
	}
	if !model.IsKind(err, model.KindParam) {
		t.Errorf("应归为参数错误（退出码 3），得到 %v", err)
	}
}

// TestChartIndexLoadsRealShapeData 断言曲目索引能从查分器真实形状的数据里建起来。
func TestChartIndexLoadsRealShapeData(t *testing.T) {
	_, baseURL := newFakeDivingFishServer(t, testMusicData, `{"records":[]}`, http.StatusOK)

	svc, err := New(Options{ChartBaseURL: baseURL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	index, err := svc.ChartIndex(context.Background())
	if err != nil {
		t.Fatalf("拉取曲目索引失败: %v", err)
	}
	if index.Len() != 2 {
		t.Fatalf("曲目数 = %d, 期望 2", index.Len())
	}
	if ds, ok := index.DS(1002, model.LevelMaster); !ok || ds != 14.0 {
		t.Errorf("定数查询不符: ds=%v ok=%v", ds, ok)
	}
	if !index.IsNew(1002) {
		t.Error("1002 应为新曲")
	}
	if index.IsNew(1001) {
		t.Error("1001 不应为新曲")
	}
}
