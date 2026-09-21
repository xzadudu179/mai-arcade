package chart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/transport"
)

// serveMusicData 启动一个假的曲目数据端点，并记录被访问次数。
func serveMusicData(t *testing.T, body string) (string, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

// newLoaderFor 构造指向指定地址的加载器。
func newLoaderFor(t *testing.T, baseURL string, cache *Cache) *Loader {
	t.Helper()
	hc, err := transport.New(transport.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造传输客户端失败: %v", err)
	}
	loader, err := NewLoader(LoaderOptions{HTTP: hc, BaseURL: baseURL, Cache: cache})
	if err != nil {
		t.Fatalf("构造加载器失败: %v", err)
	}
	return loader
}

// TestLoaderUsesCacheOnSecondCall 断言第二次加载命中缓存、不再回源。
func TestLoaderUsesCacheOnSecondCall(t *testing.T) {
	baseURL, calls := serveMusicData(t, sampleMusicData)
	dir := t.TempDir()

	cache := &Cache{Dir: dir, TTL: time.Hour}
	loader := newLoaderFor(t, baseURL, cache)

	first, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	if first.Len() != 4 {
		t.Fatalf("曲目数 = %d, 期望 4", first.Len())
	}
	if calls.Load() != 1 {
		t.Fatalf("回源次数 = %d, 期望 1", calls.Load())
	}

	second, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if second.Len() != 4 {
		t.Errorf("缓存加载的曲目数 = %d, 期望 4", second.Len())
	}
	if calls.Load() != 1 {
		t.Errorf("回源次数 = %d, 期望仍为 1（应命中缓存）", calls.Load())
	}
}

// TestLoaderRefetchesAfterTTL 断言缓存过期后回源。
func TestLoaderRefetchesAfterTTL(t *testing.T) {
	baseURL, calls := serveMusicData(t, sampleMusicData)
	dir := t.TempDir()

	now := time.Now()
	cache := &Cache{
		Dir: dir,
		TTL: time.Minute,
		Now: func() time.Time { return now },
	}
	loader := newLoaderFor(t, baseURL, cache)

	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("回源次数 = %d, 期望 1", calls.Load())
	}

	// 推过 TTL：文件修改时间仍是真实时间，因此把「现在」推到未来。
	now = now.Add(2 * time.Minute)
	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("过期后加载失败: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("回源次数 = %d, 期望 2（缓存过期应回源）", calls.Load())
	}
}

// TestCacheRejectsCorruptContent 断言损坏的缓存被当作未命中，而不是让主流程失败。
func TestCacheRejectsCorruptContent(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"不是 JSON", "<html>维护中</html>"},
		{"空数组", "[]"},
		{"对象而非数组", `{"songs":[]}`},
		{"截断的内容", `[{"id":"8","title":"X"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseURL, calls := serveMusicData(t, sampleMusicData)
			dir := t.TempDir()

			// 预置一份损坏的缓存。
			if err := os.WriteFile(filepath.Join(dir, cacheFileName), []byte(tc.body), 0o600); err != nil {
				t.Fatalf("写缓存失败: %v", err)
			}

			loader := newLoaderFor(t, baseURL, &Cache{Dir: dir, TTL: time.Hour})
			index, err := loader.Load(context.Background())
			if err != nil {
				t.Fatalf("损坏缓存不应让加载失败: %v", err)
			}
			if index.Len() != 4 {
				t.Errorf("曲目数 = %d, 期望回源拿到 4", index.Len())
			}
			if calls.Load() != 1 {
				t.Errorf("回源次数 = %d, 期望 1", calls.Load())
			}
		})
	}
}

// TestCacheHandlesFutureTimestamp 断言修改时间在未来时按过期处理。
//
// 复制文件或校时都会造成这种情况；宁可多回源一次，也不要长期用可能过时的数据。
func TestCacheHandlesFutureTimestamp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	cache := &Cache{Dir: dir, TTL: time.Hour, Now: func() time.Time { return now }}
	if err := os.WriteFile(cache.path(), []byte(sampleMusicData), 0o600); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}
	// 把文件时间设到未来。
	future := now.Add(48 * time.Hour)
	if err := os.Chtimes(cache.path(), future, future); err != nil {
		t.Fatalf("设置文件时间失败: %v", err)
	}

	if _, ok := cache.Load(); ok {
		t.Error("修改时间在未来时不应命中缓存")
	}
}

// TestCacheDisabledWhenDirEmpty 断言目录为空即不缓存。
func TestCacheDisabledWhenDirEmpty(t *testing.T) {
	baseURL, calls := serveMusicData(t, sampleMusicData)
	loader := newLoaderFor(t, baseURL, &Cache{})

	for i := 0; i < 3; i++ {
		if _, err := loader.Load(context.Background()); err != nil {
			t.Fatalf("第 %d 次加载失败: %v", i+1, err)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("回源次数 = %d, 期望 3（未启用缓存）", calls.Load())
	}
}

// TestNilCacheIsSafe 断言 nil 缓存不会 panic。
func TestNilCacheIsSafe(t *testing.T) {
	baseURL, calls := serveMusicData(t, sampleMusicData)
	loader := newLoaderFor(t, baseURL, nil)

	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("回源次数 = %d, 期望 1", calls.Load())
	}

	var cache *Cache
	if body, ok := cache.Load(); ok || body != nil {
		t.Error("nil 缓存不应命中")
	}
	cache.Store([]byte("x"))
	if err := cache.Invalidate(); err != nil {
		t.Errorf("nil 缓存删除不应报错: %v", err)
	}
}

// TestCacheStoreIsAtomic 断言写入不会留下半截文件。
//
// 先写临时文件再改名：并发读取方永远看不到写了一半的内容。
func TestCacheStoreIsAtomic(t *testing.T) {
	dir := t.TempDir()
	cache := &Cache{Dir: dir, TTL: time.Hour}

	cache.Store([]byte(sampleMusicData))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != cacheFileName {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("目录内容 = %v, 期望只有 %s（不应残留临时文件）", names, cacheFileName)
	}

	body, ok := cache.Load()
	if !ok {
		t.Fatal("写入后应当能命中缓存")
	}
	var songs []json.RawMessage
	if err := json.Unmarshal(body, &songs); err != nil {
		t.Fatalf("缓存内容不是合法 JSON: %v", err)
	}
}

// TestCacheInvalidate 断言可以强制回源。
func TestCacheInvalidate(t *testing.T) {
	baseURL, calls := serveMusicData(t, sampleMusicData)
	dir := t.TempDir()
	cache := &Cache{Dir: dir, TTL: time.Hour}
	loader := newLoaderFor(t, baseURL, cache)

	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := cache.Invalidate(); err != nil {
		t.Fatalf("删除缓存失败: %v", err)
	}
	if _, err := loader.Load(context.Background()); err != nil {
		t.Fatalf("删除后加载失败: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("回源次数 = %d, 期望 2", calls.Load())
	}
}
