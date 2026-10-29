package cache

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type UploadCache struct {
	dbPath string
	db     *sql.DB
	mu     sync.RWMutex
}

func OpenCache(dbPath string) (*UploadCache, error) {
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create cache directory %s: %w", dir, err)
		}
	}

	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", dbPath)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	db.SetMaxOpenConns(1) // SQLite works best with 1 writer connection in pure Go

	cache := &UploadCache{
		dbPath: dbPath,
		db:     db,
	}

	if err := cache.createTables(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return cache, nil
}

func (c *UploadCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		_, _ = c.db.Exec("PRAGMA wal_checkpoint(PASSIVE);")
		err := c.db.Close()
		c.db = nil
		return err
	}
	return nil
}

func (c *UploadCache) createTables() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	schema := `
	CREATE TABLE IF NOT EXISTS uploaded_files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_path TEXT NOT NULL,
		file_name TEXT NOT NULL,
		file_size INTEGER NOT NULL,
		mtime REAL NOT NULL,
		cloud_id INTEGER,
		uploaded_at TEXT NOT NULL,
		status TEXT NOT NULL,
		checksum TEXT,
		UNIQUE(file_path, file_size)
	);
	CREATE INDEX IF NOT EXISTS idx_name_size ON uploaded_files(file_name, file_size);
	CREATE INDEX IF NOT EXISTS idx_file_path ON uploaded_files(file_path);
	`
	if _, err := c.db.Exec(schema); err != nil {
		return fmt.Errorf("failed creating schema: %w", err)
	}

	// Safe migration check for checksum column
	hasChecksum := false
	rows, err := c.db.Query("PRAGMA table_info(uploaded_files)")
	if err == nil {
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dfltValue interface{}
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
				if strings.ToLower(name) == "checksum" {
					hasChecksum = true
					break
				}
			}
		}
		rows.Close()
	}

	if !hasChecksum {
		_, _ = c.db.Exec("ALTER TABLE uploaded_files ADD COLUMN checksum TEXT")
	}

	_, _ = c.db.Exec("CREATE INDEX IF NOT EXISTS idx_checksum ON uploaded_files(checksum)")
	return nil
}

func CalculateFileMD5(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	buf := make([]byte, 1024*1024) // 1 MB buffer
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *UploadCache) IsUploaded(filePath string, checkChecksum bool) bool {
	fi, err := os.Stat(filePath)
	if err != nil {
		return false
	}
	size := fi.Size()
	name := fi.Name()
	relPath := filePath

	// 1. Fast indexed check under read lock
	c.mu.RLock()
	var dummyID int64
	row := c.db.QueryRow("SELECT id FROM uploaded_files WHERE file_path = ? AND file_size = ? AND status = 'ok' LIMIT 1", relPath, size)
	if err := row.Scan(&dummyID); err == nil {
		c.mu.RUnlock()
		return true
	}

	row = c.db.QueryRow("SELECT id FROM uploaded_files WHERE file_name = ? AND file_size = ? AND status = 'ok' LIMIT 1", name, size)
	if err := row.Scan(&dummyID); err == nil {
		c.mu.RUnlock()
		return true
	}
	c.mu.RUnlock()

	// 2. Twin detection via MD5 checksum (calculated outside mutex lock!)
	if checkChecksum {
		fileMD5, err := CalculateFileMD5(filePath)
		if err == nil && fileMD5 != "" {
			c.mu.RLock()
			row = c.db.QueryRow("SELECT id FROM uploaded_files WHERE checksum = ? AND status = 'ok' LIMIT 1", fileMD5)
			if err := row.Scan(&dummyID); err == nil {
				c.mu.RUnlock()
				return true
			}
			c.mu.RUnlock()
		}
	}

	return false
}

func (c *UploadCache) MarkUploaded(filePath string, cloudID *int64, status string, checksum string) error {
	fi, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	size := fi.Size()
	name := fi.Name()
	mtime := float64(fi.ModTime().UnixNano()) / 1e9
	relPath := filePath
	now := time.Now().UTC().Format(time.RFC3339)

	c.mu.Lock()
	defer c.mu.Unlock()

	query := `
	INSERT INTO uploaded_files (file_path, file_name, file_size, mtime, cloud_id, uploaded_at, status, checksum)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(file_path, file_size) DO UPDATE SET
		cloud_id = excluded.cloud_id,
		uploaded_at = excluded.uploaded_at,
		status = excluded.status,
		checksum = COALESCE(excluded.checksum, uploaded_files.checksum)
	`
	_, err = c.db.Exec(query, relPath, name, size, mtime, cloudID, now, status, checksum)
	return err
}

type CloudItemInfo struct {
	Name    string
	Size    int64
	CloudID int64
}

func (c *UploadCache) ImportCloudItems(items []CloudItemInfo) error {
	if len(items) == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
	INSERT OR IGNORE INTO uploaded_files (file_path, file_name, file_size, mtime, cloud_id, uploaded_at, status)
	VALUES (?, ?, ?, 0.0, ?, ?, 'ok')
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, it := range items {
		_, _ = stmt.Exec(it.Name, it.Name, it.Size, it.CloudID, now)
	}

	return tx.Commit()
}
