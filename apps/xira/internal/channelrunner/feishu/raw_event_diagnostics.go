package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	rawEventDiagnosticsDirName = "raw-event-diagnostics"
	rawEventFilePrefix         = "im-message-receive-v1-"
	rawEventFileSuffix         = ".jsonl"
)

var errRawEventCapacityReached = errors.New("raw event diagnostic capacity reached")

type rawEventRecorder struct {
	mu            sync.Mutex
	dir           string
	entrypointID  string
	maxBytes      int64
	retention     time.Duration
	activePath    string
	now           func() time.Time
	pruneInterval time.Duration
}

// RunRetention enforces expiry while the runner is alive even when no new
// message arrives. Capture also prunes synchronously, so capacity accounting
// and retention remain correct before the janitor's first tick.
func (r *rawEventRecorder) RunRetention(ctx context.Context) {
	if r == nil {
		return
	}
	interval := r.pruneInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.pruneExpired(r.now().UTC()); err != nil {
			slog.Warn("feishu raw event diagnostic retention cleanup failed",
				"entrypoint_id", r.entrypointID,
				"error", err,
			)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type rawEventRecord struct {
	CapturedAt   string          `json:"captured_at"`
	EntrypointID string          `json:"entrypoint_id"`
	ChatID       string          `json:"chat_id"`
	Payload      json.RawMessage `json:"payload"`
}

func newRawEventRecorder(stateDir, entrypointID string, maxBytes int64, retention time.Duration) (*rawEventRecorder, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("raw event diagnostics max_bytes must be greater than zero")
	}
	if retention <= 0 {
		return nil, fmt.Errorf("raw event diagnostics retention_hours must be greater than zero")
	}
	return &rawEventRecorder{
		dir:          filepath.Join(stateDir, rawEventDiagnosticsDirName),
		entrypointID: strings.TrimSpace(entrypointID),
		maxBytes:     maxBytes,
		retention:    retention,
		now:          time.Now,
	}, nil
}

// captureRawMessageReceive deliberately runs before typed-event validation and
// before authorization/mention gates: when enabled, every receive_v1 payload
// for this entrypoint is evidence, including malformed or ignored messages.
// Capture is diagnostic-only; failure is visible but never fails the SDK
// handler and therefore never changes normal message processing.
func (r *Runner) captureRawMessageReceive(event *larkim.P2MessageReceiveV1) {
	if r == nil || r.rawEvents == nil {
		return
	}
	var payload []byte
	var chatID string
	if event != nil {
		if event.EventReq != nil {
			payload = event.EventReq.Body
		}
		if event.Event != nil && event.Event.Message != nil {
			chatID = stringValue(event.Event.Message.ChatId)
		}
	}
	if len(payload) == 0 {
		slog.Warn("feishu raw event diagnostic capture failed",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"error", "SDK event did not include EventReq.Body",
		)
		return
	}
	if err := r.rawEvents.Capture(chatID, payload); err != nil {
		slog.Warn("feishu raw event diagnostic capture failed",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"error", err,
		)
	}
}

func (r *rawEventRecorder) Capture(chatID string, payload []byte) error {
	if r == nil {
		return nil
	}
	now := r.now().UTC()
	recordBytes, err := json.Marshal(rawEventRecord{
		CapturedAt:   now.Format(time.RFC3339Nano),
		EntrypointID: r.entrypointID,
		ChatID:       strings.TrimSpace(chatID),
		Payload:      json.RawMessage(payload),
	})
	if err != nil {
		return fmt.Errorf("encode raw event diagnostic record: %w", err)
	}
	recordBytes = append(recordBytes, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	usedBytes, err := r.prepareDirectoryLocked(now)
	if err != nil {
		return err
	}
	if int64(len(recordBytes)) > r.maxBytes-usedBytes {
		return fmt.Errorf("%w: used_bytes=%d record_bytes=%d max_bytes=%d", errRawEventCapacityReached, usedBytes, len(recordBytes), r.maxBytes)
	}
	if r.activePath == "" {
		r.activePath = filepath.Join(r.dir, fmt.Sprintf("%s%d%s", rawEventFilePrefix, now.UnixNano(), rawEventFileSuffix))
	}
	return appendRawEventRecord(r.activePath, recordBytes)
}

func (r *rawEventRecorder) pruneExpired(now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := os.Stat(r.dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat raw event diagnostic directory: %w", err)
	}
	_, err := r.prepareDirectoryLocked(now)
	return err
}

func (r *rawEventRecorder) prepareDirectoryLocked(now time.Time) (int64, error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return 0, fmt.Errorf("create raw event diagnostic directory: %w", err)
	}
	if err := os.Chmod(r.dir, 0o700); err != nil {
		return 0, fmt.Errorf("secure raw event diagnostic directory: %w", err)
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return 0, fmt.Errorf("scan raw event diagnostic directory: %w", err)
	}
	cutoff := now.Add(-r.retention)
	var usedBytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), rawEventFilePrefix) || !strings.HasSuffix(entry.Name(), rawEventFileSuffix) {
			continue
		}
		path := filepath.Join(r.dir, entry.Name())
		startedAt, parsed := rawEventFileStart(entry.Name())
		if parsed && !startedAt.After(cutoff) {
			if err := os.Remove(path); err != nil {
				return 0, fmt.Errorf("remove expired raw event diagnostic file: %w", err)
			}
			if path == r.activePath {
				r.activePath = ""
			}
			slog.Warn("feishu raw event diagnostic retention limit reached; expired file removed",
				"entrypoint_id", r.entrypointID,
				"file", path,
				"retention", r.retention.String(),
			)
			continue
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return 0, fmt.Errorf("secure raw event diagnostic file: %w", err)
		}
		info, err := entry.Info()
		if err != nil {
			return 0, fmt.Errorf("stat raw event diagnostic file: %w", err)
		}
		usedBytes += info.Size()
	}
	return usedBytes, nil
}

func rawEventFileStart(name string) (time.Time, bool) {
	raw := strings.TrimSuffix(strings.TrimPrefix(name, rawEventFilePrefix), rawEventFileSuffix)
	nanoseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, nanoseconds).UTC(), true
}

func appendRawEventRecord(path string, record []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open raw event diagnostic file: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure raw event diagnostic file: %w", err)
	}
	written, err := file.Write(record)
	if err != nil {
		return fmt.Errorf("write raw event diagnostic record: %w", err)
	}
	if written != len(record) {
		return fmt.Errorf("write raw event diagnostic record: %w", io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync raw event diagnostic record: %w", err)
	}
	return nil
}
