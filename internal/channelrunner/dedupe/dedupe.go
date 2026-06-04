package dedupe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type MessageDeduper struct {
	mu      sync.Mutex
	ttl     time.Duration
	path    string
	entries map[string]entry
}

type entry struct {
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

func New(path string, ttl time.Duration) *MessageDeduper {
	if ttl <= 0 {
		ttl = time.Hour
	}
	d := &MessageDeduper{
		ttl:     ttl,
		path:    strings.TrimSpace(path),
		entries: map[string]entry{},
	}
	_ = d.load()
	return d
}

func (d *MessageDeduper) Begin(key string, now time.Time) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked(now)
	if _, ok := d.entries[key]; ok {
		_ = d.persistLocked()
		return false
	}
	d.entries[key] = entry{Status: "processing", ExpiresAt: now.Add(d.ttl)}
	_ = d.persistLocked()
	return true
}

func (d *MessageDeduper) Complete(key string, now time.Time) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[key] = entry{Status: "completed", ExpiresAt: now.Add(d.ttl)}
	_ = d.persistLocked()
}

func (d *MessageDeduper) Forget(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
	_ = d.persistLocked()
}

func (d *MessageDeduper) load() error {
	if d.path == "" {
		return nil
	}
	data, err := os.ReadFile(d.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var entries map[string]entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	now := time.Now()
	for key, value := range entries {
		if key == "" || !value.ExpiresAt.After(now) {
			continue
		}
		if value.Status == "" {
			value.Status = "completed"
		}
		d.entries[key] = value
	}
	return nil
}

func (d *MessageDeduper) persistLocked() error {
	if d.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.path, append(data, '\n'), 0o644)
}

func (d *MessageDeduper) pruneLocked(now time.Time) {
	for key, value := range d.entries {
		if !value.ExpiresAt.After(now) {
			delete(d.entries, key)
		}
	}
}
