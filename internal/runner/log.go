package runner

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Logger writes one JSON object per line. Cloud Logging reads "severity" and
// "message" from these, so the job's logs are searchable by field.
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewLogger returns a Logger writing to w.
func NewLogger(w io.Writer) *Logger { return &Logger{w: w, now: time.Now} }

func (l *Logger) Info(msg string, kv ...string)  { l.log("INFO", msg, kv) }
func (l *Logger) Warn(msg string, kv ...string)  { l.log("WARNING", msg, kv) }
func (l *Logger) Error(msg string, kv ...string) { l.log("ERROR", msg, kv) }

func (l *Logger) log(severity, msg string, kv []string) {
	if l == nil {
		return
	}
	entry := map[string]string{"severity": severity, "message": msg, "time": l.now().UTC().Format(time.RFC3339Nano)}
	for i := 0; i+1 < len(kv); i += 2 {
		entry[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(entry)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}
