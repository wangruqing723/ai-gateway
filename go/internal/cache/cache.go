// Package cache 对齐 Node 版 lib/cache.js：SQLite 图片识别结果缓存。
//
// Node 版用 sql.js（wasm + 整库读写），Go 版直接用纯 Go 的 modernc.org/sqlite 驱动
// （CGO_ENABLED=0 可编译，保持静态二进制 + distroless）。用单一互斥串行化 DB 操作，
// 对齐 Node 版 withDB 队列语义。时间戳沿用 SQLite 的 strftime('%s','now')（unix 秒）。
package cache

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

const dbPath = "data/image-cache.db"

// Cache SQLite 图片缓存
type Cache struct {
	mu sync.Mutex
	db *sql.DB
}

// Stats 缓存统计
type Stats struct {
	Total       int   `json:"total"`
	ContentSize int64 `json:"contentSize"`
	DBSize      int64 `json:"dbSize"`
}

// CleanupResult 清理结果
type CleanupResult struct {
	Deleted   int64
	Remaining int
}

// Open 初始化数据库并建表。
func Open() (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// modernc sqlite 单写者：限制单连接串行化，避免 "database is locked"
	db.SetMaxOpenConns(1)

	schema := `
		CREATE TABLE IF NOT EXISTS image_cache (
			hash TEXT PRIMARY KEY,
			description TEXT NOT NULL,
			created_at INTEGER DEFAULT (strftime('%s','now')),
			last_used_at INTEGER DEFAULT (strftime('%s','now'))
		);
		CREATE INDEX IF NOT EXISTS idx_created_at ON image_cache(created_at);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Cache{db: db}, nil
}

// ImageHash 生成图片哈希（SHA-256 前 32 字符），对齐 Node 版 getImageHash。
// imageBlock 为 Anthropic 格式的 image 块（map）。
func ImageHash(imageBlock map[string]any) string {
	src, _ := imageBlock["source"].(map[string]any)
	var raw string
	if src != nil {
		if t, _ := src["type"].(string); t == "base64" {
			mediaType, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			raw = mediaType + "_" + data
		} else {
			if u, ok := src["url"].(string); ok {
				raw = u
			} else if iu, ok := src["image_url"].(map[string]any); ok {
				raw, _ = iu["url"].(string)
			}
		}
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:32]
}

// Get 查询缓存描述，命中则更新 last_used_at。未命中返回 ("", false)。
func (c *Cache) Get(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var desc string
	err := c.db.QueryRow("SELECT description FROM image_cache WHERE hash = ?", hash).Scan(&desc)
	if err != nil {
		return "", false
	}
	_, _ = c.db.Exec("UPDATE image_cache SET last_used_at = strftime('%s','now') WHERE hash = ?", hash)
	return desc, true
}

// Set 存入缓存（覆盖同 hash）。
func (c *Cache) Set(hash, description string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.db.Exec(
		"INSERT OR REPLACE INTO image_cache (hash, description, created_at, last_used_at) VALUES (?, ?, strftime('%s','now'), strftime('%s','now'))",
		hash, description,
	)
	return err
}

// Cleanup 清理过期记录 + 仅保留最新 N 条，对齐 Node 版 cleanupCache。
func (c *Cache) Cleanup(maxAgeDays, maxRecords int) (CleanupResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	maxAgeSeconds := int64(maxAgeDays) * 24 * 60 * 60
	r1, err := c.db.Exec("DELETE FROM image_cache WHERE created_at < strftime('%s','now') - ?", maxAgeSeconds)
	if err != nil {
		return CleanupResult{}, err
	}
	deletedByAge, _ := r1.RowsAffected()

	r2, err := c.db.Exec(`DELETE FROM image_cache WHERE hash NOT IN (
		SELECT hash FROM image_cache ORDER BY created_at DESC LIMIT ?)`, maxRecords)
	if err != nil {
		return CleanupResult{}, err
	}
	deletedByCount, _ := r2.RowsAffected()

	total := deletedByAge + deletedByCount
	if total > 0 {
		_, _ = c.db.Exec("VACUUM")
	}

	var remaining int
	_ = c.db.QueryRow("SELECT COUNT(*) FROM image_cache").Scan(&remaining)
	return CleanupResult{Deleted: total, Remaining: remaining}, nil
}

// GetStats 返回缓存统计，对齐 Node 版 getCacheStats。
func (c *Cache) GetStats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()

	var s Stats
	_ = c.db.QueryRow("SELECT COUNT(*) FROM image_cache").Scan(&s.Total)
	if s.Total > 0 {
		_ = c.db.QueryRow("SELECT COALESCE(SUM(LENGTH(description)),0) FROM image_cache").Scan(&s.ContentSize)
	}
	if fi, err := os.Stat(dbPath); err == nil {
		s.DBSize = fi.Size()
	}
	return s
}

// Close 关闭数据库。
func (c *Cache) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.Close()
}
