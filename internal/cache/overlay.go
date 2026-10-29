package cache

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type OverlayEntry struct {
	Name      string
	Size      int64
	CloudID   int64
	ExpiresAt time.Time
	Deleted   bool
}

type OverlayStore struct {
	mu      sync.RWMutex
	entries map[string]OverlayEntry
	ttl     time.Duration
}

func NewOverlayStore(ttl time.Duration) *OverlayStore {
	if ttl <= 0 {
		ttl = 15 * time.Minute // 900 seconds default
	}
	return &OverlayStore{
		entries: make(map[string]OverlayEntry),
		ttl:     ttl,
	}
}

func (o *OverlayStore) AddUploaded(filePath string, size int64, cloudID int64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	name := filepath.Base(filePath)
	entry := OverlayEntry{
		Name:      name,
		Size:      size,
		CloudID:   cloudID,
		ExpiresAt: time.Now().Add(o.ttl),
		Deleted:   false,
	}
	o.entries[strings.ToLower(name)] = entry
	o.entries[strings.ToLower(filePath)] = entry
}

func (o *OverlayStore) AddDeletedTombstone(filePath string, ttl time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if ttl <= 0 {
		ttl = 5 * time.Minute // 300 seconds default
	}
	name := filepath.Base(filePath)
	entry := OverlayEntry{
		Name:      name,
		ExpiresAt: time.Now().Add(ttl),
		Deleted:   true,
	}
	o.entries[strings.ToLower(name)] = entry
	o.entries[strings.ToLower(filePath)] = entry
}

func (o *OverlayStore) Check(filePath string, size int64) (isUploaded bool, isDeleted bool, cloudID int64) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	now := time.Now()
	name := filepath.Base(filePath)

	keys := []string{strings.ToLower(filePath), strings.ToLower(name)}
	for _, k := range keys {
		if entry, ok := o.entries[k]; ok {
			if now.After(entry.ExpiresAt) {
				continue
			}
			if entry.Deleted {
				return false, true, 0
			}
			if size <= 0 || entry.Size == size {
				return true, false, entry.CloudID
			}
		}
	}
	return false, false, 0
}
