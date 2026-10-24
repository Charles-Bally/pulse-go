package pulse

import (
	"io"
	"strings"
	"sync"
	"time"
)

const (
	maxBufferedLogs = 1000
	maxMessageChars = 4000
	maxContextChars = 80
)

type logEntry struct {
	At      string `json:"at"`
	Level   string `json:"level"`
	Context string `json:"context,omitempty"`
	Message string `json:"message"`
}

func newEntry(at time.Time, level, context, message string) logEntry {
	if at.IsZero() {
		at = time.Now()
	}
	message = truncate(redactSecrets(strings.TrimSpace(message)), maxMessageChars)
	if message == "" {
		message = "(vide)"
	}
	return logEntry{At: isoTime(at), Level: level, Context: truncate(context, maxContextChars), Message: message}
}

type collector struct {
	mu       sync.Mutex
	buffer   []logEntry
	minLevel int
}

func (c *collector) push(entry logEntry) {
	if levelIndex(entry.Level) < c.minLevel {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer = append(c.buffer, entry)
	if overflow := len(c.buffer) - maxBufferedLogs; overflow > 0 {
		c.buffer = append([]logEntry(nil), c.buffer[overflow:]...)
	}
}

func (c *collector) take(limit int) []logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := min(limit, len(c.buffer))
	taken := append([]logEntry(nil), c.buffer[:count]...)
	c.buffer = append([]logEntry(nil), c.buffer[count:]...)
	return taken
}

func (c *collector) putBack(entries []logEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	merged := append(append([]logEntry(nil), entries...), c.buffer...)
	if overflow := len(merged) - maxBufferedLogs; overflow > 0 {
		merged = merged[overflow:]
	}
	c.buffer = merged
}

func (c *collector) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer = nil
}

func (c *collector) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buffer)
}

func (r *Reporter) capturing() bool { return r != nil && r.logsOn.Load() }

// Une ligne illisible ne doit jamais casser l'écriture de l'app.
func (r *Reporter) capture(build func() (logEntry, bool)) {
	if !r.capturing() {
		return
	}
	_ = safeRun(func() error {
		if entry, ok := build(); ok {
			r.collector.push(entry)
		}
		return nil
	})
}

type logWriter struct {
	dst      io.Writer
	reporter *Reporter
}

// Avertissement : l'écriture d'origine passe toujours en premier, une panne de Pulse ne doit rien cacher.
func (w logWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.reporter.capture(func() (logEntry, bool) { return parseLine(string(p)) })
	return n, err
}

func (r *Reporter) Writer(dst io.Writer) io.Writer {
	if dst == nil {
		dst = io.Discard
	}
	if r == nil {
		return dst
	}
	return logWriter{dst: dst, reporter: r}
}
