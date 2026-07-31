package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/xiramesh/xira/internal/entrypoints"
)

func TestRawEventDiagnosticsPreservesSDKUnknownFieldsAcrossChats(t *testing.T) {
	stateDir := t.TempDir()
	recorder, err := newRawEventRecorder(stateDir, "feishu-main", 1<<20, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 7, 31, 8, 9, 10, 123, time.UTC)
	recorder.now = func() time.Time { return fixedNow }

	rawEvents := [][]byte{
		[]byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1","wire_header_extra":"keep-me"},"event":{"sender":{"sender_id":{"open_id":"ou_a"},"name":"Alice","wire_sender_extra":{"level":7}},"message":{"chat_id":"oc_a","message_id":"om_a","chat_type":"group","message_type":"text","content":"{\"text\":\"one\"}"}},"top_level_extra":["x",2]}`),
		[]byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1"},"event":{"sender":{"sender_id":{"open_id":"ou_b"},"name":"Bob"},"message":{"chat_id":"oc_b","message_id":"om_b","chat_type":"group","message_type":"text","content":"{\"text\":\"two\"}"}},"another_unknown":true}`),
	}
	runner := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, rawEvents: recorder}
	dispatcher := larkdispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(
		func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
			runner.captureRawMessageReceive(event)
			return nil
		},
	)
	for _, raw := range rawEvents {
		if _, err := dispatcher.Do(context.Background(), raw); err != nil {
			t.Fatalf("SDK EventDispatcher.Do: %v", err)
		}
	}

	records := readRawDiagnosticRecords(t, recorder.dir)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].CapturedAt != fixedNow.Format(time.RFC3339Nano) {
		t.Fatalf("captured_at = %q", records[0].CapturedAt)
	}
	if records[0].EntrypointID != "feishu-main" || records[0].ChatID != "oc_a" {
		t.Fatalf("first record routing fields = %+v", records[0])
	}
	if records[1].ChatID != "oc_b" {
		t.Fatalf("second chat_id = %q, want oc_b", records[1].ChatID)
	}
	var firstPayload map[string]any
	if err := json.Unmarshal(records[0].Payload, &firstPayload); err != nil {
		t.Fatal(err)
	}
	eventMap := firstPayload["event"].(map[string]any)
	senderMap := eventMap["sender"].(map[string]any)
	if senderMap["name"] != "Alice" {
		t.Fatalf("unknown event.sender.name lost: %#v", senderMap)
	}
	if senderMap["wire_sender_extra"].(map[string]any)["level"] != float64(7) {
		t.Fatalf("unknown nested sender field lost: %#v", senderMap)
	}
	if _, ok := firstPayload["top_level_extra"]; !ok {
		t.Fatalf("unknown top-level field lost: %#v", firstPayload)
	}

	dirInfo, err := os.Stat(recorder.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("diagnostic dir mode = %o, want 700", got)
	}
	files, err := filepath.Glob(filepath.Join(recorder.dir, rawEventFilePrefix+"*"+rawEventFileSuffix))
	if err != nil || len(files) != 1 {
		t.Fatalf("diagnostic files = %v, err = %v", files, err)
	}
	fileInfo, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("diagnostic file mode = %o, want 600", got)
	}
}

func TestRawEventDiagnosticsConcurrentWritesRemainValidJSONL(t *testing.T) {
	recorder, err := newRawEventRecorder(t.TempDir(), "feishu-main", 4<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const count = 96
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf(`{"schema":"2.0","event":{"message":{"chat_id":"oc_%d"},"unknown":{"sequence":%d}}}`, i%2, i))
			if err := recorder.Capture(fmt.Sprintf("oc_%d", i%2), payload); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Capture: %v", err)
	}
	records := readRawDiagnosticRecords(t, recorder.dir)
	if len(records) != count {
		t.Fatalf("records = %d, want %d", len(records), count)
	}
	seen := make(map[int]bool, count)
	for _, record := range records {
		var payload struct {
			Event struct {
				Unknown struct {
					Sequence int `json:"sequence"`
				} `json:"unknown"`
			} `json:"event"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			t.Fatalf("interleaved/corrupt payload: %v", err)
		}
		seen[payload.Event.Unknown.Sequence] = true
	}
	if len(seen) != count {
		t.Fatalf("unique sequences = %d, want %d", len(seen), count)
	}
}

func TestRawEventDiagnosticsDefaultDisabledCreatesNothing(t *testing.T) {
	stateRoot := t.TempDir()
	runner, err := NewRunner(entrypoints.Definition{
		ID: "feishu-main", Channel: "feishu", AppID: "cli_test", AppSecret: "secret",
	}, nil, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schema":"2.0","event":{"message":{"chat_id":"oc_a"}}}`)
	runner.captureRawMessageReceive(&larkim.P2MessageReceiveV1{EventReq: &larkevent.EventReq{Body: raw}})
	diagnosticsDir := filepath.Join(stateRoot, "channels", "feishu", "feishu-main", rawEventDiagnosticsDirName)
	if _, err := os.Stat(diagnosticsDir); !os.IsNotExist(err) {
		t.Fatalf("disabled diagnostics created %q: %v", diagnosticsDir, err)
	}
}

func TestRawEventDiagnosticsRejectsUnsafeEnabledConfiguration(t *testing.T) {
	base := entrypoints.Definition{
		ID: "feishu-main", Channel: "feishu", AppID: "cli_test", AppSecret: "secret",
		RawEventDiagnostics: &entrypoints.RawEventDiagnosticsPolicy{Enabled: true},
	}
	if _, err := NewRunner(base, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("missing max_bytes error = %v", err)
	}
	base.RawEventDiagnostics.MaxBytes = 1 << 20
	if _, err := NewRunner(base, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "retention_hours") {
		t.Fatalf("missing retention_hours error = %v", err)
	}
	base.RawEventDiagnostics.RetentionHours = 24
	runner, err := NewRunner(base, nil, t.TempDir())
	if err != nil {
		t.Fatalf("valid diagnostics config: %v", err)
	}
	if runner.rawEvents == nil {
		t.Fatal("enabled diagnostics did not configure recorder")
	}
	base.RawEventDiagnostics.RetentionHours = int(time.Duration(1<<63-1)/time.Hour) + 1
	if _, err := NewRunner(base, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "retention_hours is too large") {
		t.Fatalf("overflowing retention_hours error = %v", err)
	}
}

func TestRawEventDiagnosticsCapacityWarnsAndDoesNotFailHandler(t *testing.T) {
	recorder, err := newRawEventRecorder(t.TempDir(), "feishu-main", 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	runner := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, rawEvents: recorder}
	event := &larkim.P2MessageReceiveV1{EventReq: &larkevent.EventReq{Body: []byte(`{"event":{"message":{"chat_id":"oc_a"}}}`)}}
	if err := runner.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("handler failed because diagnostic capacity was reached: %v", err)
	}
	if !strings.Contains(logBuf.String(), "raw event diagnostic capture failed") || !strings.Contains(logBuf.String(), "capacity") {
		t.Fatalf("capacity drop was not explicitly warned: %s", logBuf.String())
	}
	if err := recorder.Capture("oc_a", event.EventReq.Body); !errors.Is(err, errRawEventCapacityReached) {
		t.Fatalf("Capture error = %v, want capacity sentinel", err)
	}
}

func TestRawEventDiagnosticsWriteFailureWarnsAndDoesNotFailHandler(t *testing.T) {
	stateDir := t.TempDir()
	diagnosticsPath := filepath.Join(stateDir, rawEventDiagnosticsDirName)
	if err := os.WriteFile(diagnosticsPath, []byte("blocks mkdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := newRawEventRecorder(stateDir, "feishu-main", 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	runner := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, rawEvents: recorder}
	event := &larkim.P2MessageReceiveV1{EventReq: &larkevent.EventReq{Body: []byte(`{"event":{"message":{"chat_id":"oc_a"}}}`)}}
	if err := runner.handleMessageReceive(context.Background(), event); err != nil {
		t.Fatalf("handler failed because diagnostic write failed: %v", err)
	}
	if !strings.Contains(logBuf.String(), "raw event diagnostic capture failed") {
		t.Fatalf("write failure was not warned: %s", logBuf.String())
	}
}

func TestRawEventDiagnosticsRetentionDeletesExpiredFilesWithWarning(t *testing.T) {
	stateDir := t.TempDir()
	recorder, err := newRawEventRecorder(stateDir, "feishu-main", 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	recorder.now = func() time.Time { return now }
	if err := os.MkdirAll(recorder.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(recorder.dir, fmt.Sprintf("%s%d%s", rawEventFilePrefix, now.Add(-2*time.Hour).UnixNano(), rawEventFileSuffix))
	if err := os.WriteFile(oldPath, []byte("old sensitive event\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	if err := recorder.Capture("oc_new", []byte(`{"event":{"message":{"chat_id":"oc_new"}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired diagnostic file still exists: %v", err)
	}
	if !strings.Contains(logBuf.String(), "retention limit reached") {
		t.Fatalf("retention deletion was not warned: %s", logBuf.String())
	}
}

func TestRawEventDiagnosticsCapacityCannotBeBypassedByRestart(t *testing.T) {
	stateDir := t.TempDir()
	first, err := newRawEventRecorder(stateDir, "feishu-main", 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Capture("oc_old", []byte(`{"event":{"message":{"chat_id":"oc_old"}}}`)); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(first.dir, rawEventFilePrefix+"*"+rawEventFileSuffix))
	if err != nil || len(files) != 1 {
		t.Fatalf("diagnostic files = %v, err = %v", files, err)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}

	restarted, err := newRawEventRecorder(stateDir, "feishu-main", info.Size(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Capture("oc_new", []byte(`{"event":{"message":{"chat_id":"oc_new"}}}`)); !errors.Is(err, errRawEventCapacityReached) {
		t.Fatalf("post-restart Capture error = %v, want existing files to count toward capacity", err)
	}
}

func TestRawEventDiagnosticsRotatesActiveFileAtRetentionBoundary(t *testing.T) {
	stateDir := t.TempDir()
	recorder, err := newRawEventRecorder(stateDir, "feishu-main", 1<<20, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	recorder.now = func() time.Time { return now }
	if err := recorder.Capture("oc_old", []byte(`{"event":{"message":{"chat_id":"oc_old"}}}`)); err != nil {
		t.Fatal(err)
	}
	oldActivePath := recorder.activePath
	now = now.Add(time.Hour)
	if err := recorder.Capture("oc_new", []byte(`{"event":{"message":{"chat_id":"oc_new"}}}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.activePath == oldActivePath {
		t.Fatalf("active path did not rotate at retention boundary: %s", recorder.activePath)
	}
	if _, err := os.Stat(oldActivePath); !os.IsNotExist(err) {
		t.Fatalf("expired active file still exists: %v", err)
	}
	records := readRawDiagnosticRecords(t, recorder.dir)
	if len(records) != 1 || records[0].ChatID != "oc_new" {
		t.Fatalf("retained records = %+v, want only oc_new", records)
	}
}

func TestRawEventDiagnosticsRetentionExpiresWithoutAnotherMessage(t *testing.T) {
	stateDir := t.TempDir()
	recorder, err := newRawEventRecorder(stateDir, "feishu-main", 1<<20, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	recorder.pruneInterval = 5 * time.Millisecond
	if err := recorder.Capture("oc_once", []byte(`{"event":{"message":{"chat_id":"oc_once"}}}`)); err != nil {
		t.Fatal(err)
	}
	path := recorder.activePath
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go recorder.RunRetention(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expired diagnostic file was not removed without a new message: %s", path)
}

type rawDiagnosticRecordForTest struct {
	CapturedAt   string          `json:"captured_at"`
	EntrypointID string          `json:"entrypoint_id"`
	ChatID       string          `json:"chat_id"`
	Payload      json.RawMessage `json:"payload"`
}

func readRawDiagnosticRecords(t *testing.T, dir string) []rawDiagnosticRecordForTest {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, rawEventFilePrefix+"*"+rawEventFileSuffix))
	if err != nil {
		t.Fatal(err)
	}
	var records []rawDiagnosticRecordForTest
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for lineNumber, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var record rawDiagnosticRecordForTest
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("%s line %d is invalid JSONL: %v\n%s", path, lineNumber+1, err, line)
			}
			records = append(records, record)
		}
	}
	return records
}
