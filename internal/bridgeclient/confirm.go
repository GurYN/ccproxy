package bridgeclient

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// TTYConfirm prompts the user on stdin/stderr for each operation and
// returns the answer. Concurrent calls are serialized so prompts don't
// interleave.
type TTYConfirm struct {
	mu sync.Mutex
	in *bufio.Reader
	w  io.Writer
}

// NewTTYConfirm wires the prompt to stderr+stdin. Returns nil if stdin
// isn't a terminal (no point prompting nobody) — caller should fall back
// to auto-approve.
func NewTTYConfirm() *TTYConfirm {
	if !isTerminal(os.Stdin) {
		return nil
	}
	return &TTYConfirm{in: bufio.NewReader(os.Stdin), w: os.Stderr}
}

// Ask blocks until the user answers y/Y or n/N. Default is no.
func (t *TTYConfirm) Ask(op, path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.w, "[ccproxy-bridge] %s %q? [y/N] ", op, path)
	line, err := t.in.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// isTerminal is a tiny replacement for golang.org/x/term to avoid the
// dep — true if fd refers to a character device, false otherwise.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
