// Package runlog reports the progress of one run. It writes short lines for
// the person at the terminal and one JSON object for each event to a log file
// that a program can read.
package runlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"pixiegrabber/internal/store"
)

// LogFilename is the log of the last run. It sits at the output root, beside
// the Collection directories.
const LogFilename = "pixiegrabber-run.log"

const (
	// maxLogBytes limits the memory that the log holds. A large account can
	// make many events, and the whole log stays in memory because the store
	// replaces a whole object and cannot append.
	maxLogBytes = 8 << 20
	// flushInterval is the time between two writes of the log.
	flushInterval = 10 * time.Second
	// redacted replaces a value that could hold a secret.
	redacted = "[redacted]"
)

// Logger collects run events. Every method is safe for concurrent use,
// because the download workers report from more than one goroutine.
type Logger struct {
	mu        sync.Mutex
	human     io.Writer
	buffer    bytes.Buffer
	store     store.Store
	clock     func() time.Time
	lastFlush time.Time
	truncated bool
	failed    bool
}

// New returns a Logger. A nil human writer stops the terminal lines and keeps
// the log file. A nil clock uses the real time.
func New(human io.Writer, clock func() time.Time) *Logger {
	if clock == nil {
		clock = time.Now
	}
	return &Logger{human: human, clock: clock, lastFlush: clock()}
}

// Attach gives the Logger a place to write. The events from before this call
// stay in memory and reach the file at the next write.
func (l *Logger) Attach(s store.Store) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.store = s
}

// Event records one event in the log file only.
func (l *Logger) Event(name string, fields map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record(name, fields)
	l.maybeFlush()
}

// Progress writes one line for the person and records the same event in the
// log file. An empty line writes nothing to the terminal.
func (l *Logger) Progress(line, name string, fields map[string]any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if line != "" && l.human != nil {
		fmt.Fprintln(l.human, line)
	}
	l.record(name, fields)
	l.maybeFlush()
}

// Print writes one line for the person and records nothing. Use it for text
// that the log already holds in another form.
func (l *Logger) Print(line string) {
	if l == nil || line == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.human != nil {
		fmt.Fprintln(l.human, line)
	}
}

// Flush writes the log now.
func (l *Logger) Flush() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flush()
}

// Close writes the log one last time.
func (l *Logger) Close() error {
	return l.Flush()
}

// record adds one event. The caller holds the lock.
func (l *Logger) record(name string, fields map[string]any) {
	if name == "" {
		return
	}
	// After the limit only a failure and the end of the run are kept, so that
	// the reason for a bad run always survives.
	if l.truncated && !alwaysKeep(name) {
		return
	}
	event := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		if key == "t" || key == "ev" {
			continue
		}
		event[safeKey(key)] = safeValue(value)
	}
	event["t"] = l.clock().UTC().Format(time.RFC3339)
	event["ev"] = safeText(name)

	// json.Marshal sorts the keys of a map, so the output is always the same.
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	if l.buffer.Len()+len(data)+1 > maxLogBytes {
		if !l.truncated {
			l.truncated = true
			l.appendLine([]byte(`{"ev":"log_truncated"}`))
		}
		if !alwaysKeep(name) {
			return
		}
	}
	l.appendLine(data)
}

func (l *Logger) appendLine(data []byte) {
	l.buffer.Write(data)
	l.buffer.WriteByte('\n')
}

// alwaysKeep reports the events that survive the memory limit.
func alwaysKeep(name string) bool {
	switch name {
	case "run_end", "download_failed", "video_stop", "error":
		return true
	default:
		return false
	}
}

// maybeFlush writes the log when enough time passed. The caller holds the
// lock. A write error stops later attempts, because the terminal already
// carries the same information.
func (l *Logger) maybeFlush() {
	if l.store == nil || l.failed {
		return
	}
	if l.clock().Sub(l.lastFlush) < flushInterval {
		return
	}
	_ = l.flush()
}

// flush writes the whole log. The caller holds the lock.
func (l *Logger) flush() error {
	if l.store == nil || l.buffer.Len() == 0 {
		return nil
	}
	data := l.buffer.Bytes()
	if err := l.store.Put(LogFilename, bytes.NewReader(data), int64(len(data)), nil); err != nil {
		l.failed = true
		return errors.New("write run log: the log could not be saved")
	}
	l.lastFlush = l.clock()
	return nil
}

// safeKey keeps a field name simple.
func safeKey(key string) string {
	if key == "" {
		return "field"
	}
	return safeText(key)
}

// safeValue cleans one field value. A string that holds a URL or a control
// character is replaced, because such a value can carry a cookie, a token or
// an S3 key. See docs/spec.md on what the tool must never save.
func safeValue(value any) any {
	switch typed := value.(type) {
	case string:
		return safeText(typed)
	case []string:
		cleaned := make([]string, len(typed))
		for i, item := range typed {
			cleaned[i] = safeText(item)
		}
		return cleaned
	case error:
		if typed == nil {
			return nil
		}
		return safeText(typed.Error())
	default:
		return value
	}
}

// safeText replaces text that could hold a secret. The "://" rule follows
// manifest.validateFailureText.
func safeText(value string) string {
	if value == "" {
		return ""
	}
	if strings.Contains(value, "://") {
		return redacted
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return redacted
	}
	if len(value) > 512 {
		return value[:512]
	}
	return value
}
