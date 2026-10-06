// Package logbuf is an io.Writer that keeps the most recent log lines in
// memory and notifies a listener of each new line, so a GUI can show history
// and tail live output.
package logbuf

import (
	"bytes"
	"strings"
	"sync"
)

// Buffer is a bounded ring of log lines. Safe for concurrent use.
type Buffer struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial []byte
	onLine  func(string)
}

// New returns a Buffer that retains at most max lines.
func New(max int) *Buffer {
	return &Buffer{max: max}
}

// OnLine sets a callback invoked (outside the lock) for every complete line.
func (b *Buffer) OnLine(fn func(string)) {
	b.mu.Lock()
	b.onLine = fn
	b.mu.Unlock()
}

// Write implements io.Writer, splitting input into lines.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.partial = append(b.partial, p...)
	var fresh []string
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(b.partial[:i]), "\r")
		b.partial = b.partial[i+1:]
		fresh = append(fresh, line)
	}
	b.lines = append(b.lines, fresh...)
	if over := len(b.lines) - b.max; over > 0 {
		b.lines = append(b.lines[:0:0], b.lines[over:]...)
	}
	fn := b.onLine
	b.mu.Unlock()

	if fn != nil {
		for _, l := range fresh {
			fn(l)
		}
	}
	return len(p), nil
}

// Lines returns a copy of the retained lines, oldest first.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}
