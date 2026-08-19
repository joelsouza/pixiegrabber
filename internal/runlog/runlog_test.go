package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore records the last write and counts them.
type fakeStore struct {
	mu     sync.Mutex
	last   []byte
	writes int
	err    error
}

func (f *fakeStore) Put(rel string, r io.Reader, size int64, metadata map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if rel != LogFilename {
		return errors.New("unexpected key " + rel)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.last = data
	f.writes++
	return nil
}

func (f *fakeStore) snapshot() ([]byte, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.last...), f.writes
}

func (f *fakeStore) Open(string) (io.ReadCloser, error)         { return nil, os.ErrNotExist }
func (f *fakeStore) Inspect(string) (os.FileInfo, bool, error)  { return nil, false, nil }
func (f *fakeStore) ReadDir(string) ([]os.DirEntry, error)      { return nil, nil }
func (f *fakeStore) Remove(string) error                        { return nil }
func (f *fakeStore) MkdirAll(string) error                      { return nil }
func (f *fakeStore) DisplayPath(rel string) (string, error)     { return rel, nil }
func (f *fakeStore) Metadata(string) (map[string]string, error) { return nil, nil }
func (f *fakeStore) SameFile(string, string) (bool, error)      { return false, nil }
func (f *fakeStore) Lock() (func() error, error)                { return func() error { return nil }, nil }
func (f *fakeStore) Close() error                               { return nil }

// fixedClock returns a clock the test controls.
func fixedClock(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}

func lines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestEventWritesOneJSONObjectForEachLine(t *testing.T) {
	now := time.Date(2026, 8, 18, 17, 22, 1, 0, time.UTC)
	store := &fakeStore{}
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)

	logger.Event("run_start", map[string]any{"mode": "local"})
	logger.Event("collection", map[string]any{"i": 12, "of": 524, "id": "117855994"})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, _ := store.snapshot()
	events := lines(t, data)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0]["ev"] != "run_start" || events[0]["mode"] != "local" {
		t.Fatalf("first event = %v", events[0])
	}
	if events[0]["t"] != "2026-08-18T17:22:01Z" {
		t.Fatalf("timestamp = %v", events[0]["t"])
	}
	if events[1]["ev"] != "collection" || events[1]["id"] != "117855994" {
		t.Fatalf("second event = %v", events[1])
	}
}

func TestProgressWritesTheTerminalLineAndTheEvent(t *testing.T) {
	now := time.Now()
	var out bytes.Buffer
	store := &fakeStore{}
	logger := New(&out, fixedClock(&now))
	logger.Attach(store)

	logger.Progress("[  1/524] Baptizado: 2 sets, 131 images", "collection",
		map[string]any{"i": 1, "of": 524})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	if got := out.String(); got != "[  1/524] Baptizado: 2 sets, 131 images\n" {
		t.Fatalf("terminal output = %q", got)
	}
	data, _ := store.snapshot()
	if events := lines(t, data); len(events) != 1 || events[0]["ev"] != "collection" {
		t.Fatalf("events = %v", events)
	}
}

func TestQuietLoggerStillWritesTheLog(t *testing.T) {
	now := time.Now()
	store := &fakeStore{}
	// A nil writer is the --quiet mode.
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)
	logger.Progress("this line is hidden", "collection", nil)
	logger.Print("this line is hidden too")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := store.snapshot()
	if events := lines(t, data); len(events) != 1 {
		t.Fatalf("events = %v, want the event without the terminal line", events)
	}
}

func TestSanitizeRemovesSecretsAndURLs(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  any
	}{
		{name: "url", value: "https://images.pixieset.com/photo.jpg", want: redacted},
		{name: "protocol relative", value: "gd://token", want: redacted},
		{name: "control character", value: "line\nbreak", want: redacted},
		{name: "tab", value: "a\tb", want: redacted},
		{name: "plain name", value: "Baptizado Maria", want: "Baptizado Maria"},
		{name: "empty", value: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeValue(tt.value); got != tt.want {
				t.Fatalf("safeValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestRecordedEventNeverHoldsAURL(t *testing.T) {
	now := time.Now()
	store := &fakeStore{}
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)
	logger.Event("download_failed", map[string]any{
		"url":   "https://images.pixieset.com/secret.jpg",
		"error": errors.New("get https://images.pixieset.com/secret.jpg: refused"),
		"code":  "source_http_status",
	})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := store.snapshot()
	if strings.Contains(string(data), "://") {
		t.Fatalf("log holds a URL: %s", data)
	}
	if !strings.Contains(string(data), "source_http_status") {
		t.Fatalf("log lost the failure code: %s", data)
	}
}

func TestCallerCannotOverwriteTheReservedFields(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)
	logger.Event("collection", map[string]any{"t": "wrong", "ev": "wrong"})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := store.snapshot()
	events := lines(t, data)
	if events[0]["ev"] != "collection" || events[0]["t"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("event = %v", events[0])
	}
}

func TestLogIsWrittenOnlyAfterTheFlushInterval(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)

	logger.Event("one", nil)
	if _, writes := store.snapshot(); writes != 0 {
		t.Fatalf("writes = %d, want none before the interval", writes)
	}
	now = now.Add(flushInterval + time.Second)
	logger.Event("two", nil)
	if _, writes := store.snapshot(); writes != 1 {
		t.Fatalf("writes = %d, want one after the interval", writes)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, writes := store.snapshot()
	if writes != 2 {
		t.Fatalf("writes = %d, want a last write from Close", writes)
	}
	if events := lines(t, data); len(events) != 2 {
		t.Fatalf("events = %v", events)
	}
}

func TestEventsBeforeAttachReachTheLog(t *testing.T) {
	now := time.Now()
	store := &fakeStore{}
	logger := New(nil, fixedClock(&now))
	// The cookie phase runs before the store exists.
	logger.Event("run_start", map[string]any{"mode": "local"})
	logger.Event("cookies", map[string]any{"browser": "chrome"})
	logger.Attach(store)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := store.snapshot()
	if events := lines(t, data); len(events) != 2 || events[0]["ev"] != "run_start" {
		t.Fatalf("events = %v", events)
	}
}

func TestLogStopsAtTheMemoryLimitAndKeepsTheEnd(t *testing.T) {
	now := time.Now()
	store := &fakeStore{}
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)

	big := strings.Repeat("a", 500)
	for i := 0; i < 40000; i++ {
		logger.Event("collection", map[string]any{"name": big})
	}
	logger.Event("run_end", map[string]any{"outcome": "completed"})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	data, _ := store.snapshot()
	if len(data) > maxLogBytes+1024 {
		t.Fatalf("log grew to %d bytes, want about %d", len(data), maxLogBytes)
	}
	text := string(data)
	if !strings.Contains(text, `"ev":"log_truncated"`) {
		t.Fatal("log does not report that it was cut")
	}
	if !strings.Contains(text, `"outcome":"completed"`) {
		t.Fatal("log lost the end of the run")
	}
}

func TestConcurrentReportsAreSafe(t *testing.T) {
	now := time.Now()
	store := &fakeStore{}
	logger := New(io.Discard, fixedClock(&now))
	logger.Attach(store)

	var group sync.WaitGroup
	// The download workers report at the same time.
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(id int) {
			defer group.Done()
			for i := 0; i < 200; i++ {
				logger.Event("download", map[string]any{"worker": id, "done": i})
				logger.Progress("", "download_failed", map[string]any{"code": "timeout"})
			}
		}(worker)
	}
	group.Wait()
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := store.snapshot()
	if events := lines(t, data); len(events) != 8*200*2 {
		t.Fatalf("events = %d, want %d", len(events), 8*200*2)
	}
}

func TestAWriteErrorDoesNotStopTheRun(t *testing.T) {
	now := time.Now()
	store := &fakeStore{err: errors.New("disk is full")}
	logger := New(nil, fixedClock(&now))
	logger.Attach(store)
	logger.Event("run_start", nil)

	err := logger.Close()
	if err == nil {
		t.Fatal("Close() hid the write error")
	}
	if strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("error repeats the backend text: %v", err)
	}
	// A later report must not panic after the failure.
	logger.Event("run_end", nil)
}

func TestNilLoggerIsSafe(t *testing.T) {
	var logger *Logger
	logger.Event("run_start", nil)
	logger.Progress("line", "run_start", nil)
	logger.Print("line")
	logger.Attach(nil)
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
