package chart

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultCacheTTL 是曲目缓存的有效期。
//
// 曲目定数与新旧曲标记随游戏版本更新才变化，按天缓存足够，也不会让玩家看到过期很久的数据。
const DefaultCacheTTL = 24 * time.Hour

// cacheFileName 是缓存文件名，内容就是上游返回的原始 JSON。
const cacheFileName = "music_data.json"

// Cache 把曲目数据缓存到本地文件。
//
// 缓存的是上游原始响应而不是自定义格式：这样文件可以直接与接口对照，
// 也复用同一套解析逻辑，不必维护第二种格式。
type Cache struct {
	// Dir 是缓存目录；为空表示不缓存。
	Dir string

	// TTL 是缓存有效期；为零取 DefaultCacheTTL。
	TTL time.Duration

	// Now 提供当前时间，便于测试注入；为空取 time.Now。
	Now func() time.Time

	// Logger 记录缓存命中与回源，nil 表示不记录。
	Logger *slog.Logger
}

// enabled 报告是否启用了缓存。
func (c *Cache) enabled() bool { return c != nil && c.Dir != "" }

// ttl 返回有效期。
func (c *Cache) ttl() time.Duration {
	if c.TTL <= 0 {
		return DefaultCacheTTL
	}
	return c.TTL
}

// now 返回当前时间。
func (c *Cache) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// path 返回缓存文件路径。
func (c *Cache) path() string { return filepath.Join(c.Dir, cacheFileName) }

// Load 读取仍有效的缓存；未命中时返回 false。
//
// 任何读取或解析问题都当作未命中：缓存只是加速手段，不能因为它让主流程失败。
func (c *Cache) Load() ([]byte, bool) {
	if !c.enabled() {
		return nil, false
	}

	info, err := os.Stat(c.path())
	if err != nil {
		return nil, false
	}

	// 用文件的修改时间当作抓取时间：省掉一个副作用文件，代价是复制文件会改变时间戳，
	// 而那种情况下最坏的后果只是多回源一次。
	age := c.now().Sub(info.ModTime())
	if age < 0 || age > c.ttl() {
		return nil, false
	}

	body, err := os.ReadFile(c.path())
	if err != nil {
		return nil, false
	}
	// 校验内容确实是曲目数组，避免把半截文件或别的东西当成缓存用。
	var probe []json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil || len(probe) == 0 {
		return nil, false
	}

	c.log("曲目缓存命中", "路径", c.path(), "条数", len(probe), "年龄", age.Round(time.Second).String())
	return body, true
}

// Store 原子写入缓存；失败只记日志，不影响调用方。
func (c *Cache) Store(body []byte) {
	if !c.enabled() {
		return
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		c.log("创建缓存目录失败", "原因", err.Error())
		return
	}

	// 先写临时文件再改名：避免读到写了一半的缓存。
	tmp, err := os.CreateTemp(c.Dir, cacheFileName+".tmp*")
	if err != nil {
		c.log("创建临时文件失败", "原因", err.Error())
		return
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		c.log("写入缓存失败", "原因", err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		c.log("关闭临时文件失败", "原因", err.Error())
		return
	}
	if err := os.Rename(tmpName, c.path()); err != nil {
		os.Remove(tmpName)
		c.log("替换缓存文件失败", "原因", err.Error())
		return
	}
	c.log("曲目缓存已更新", "路径", c.path(), "字节", len(body))
}

// Invalidate 删除缓存文件，用于强制回源。
func (c *Cache) Invalidate() error {
	if !c.enabled() {
		return nil
	}
	if err := os.Remove(c.path()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除曲目缓存: %w", err)
	}
	return nil
}

// log 输出调试日志。
func (c *Cache) log(msg string, attrs ...any) {
	if c.Logger != nil {
		c.Logger.Debug(msg, attrs...)
	}
}
