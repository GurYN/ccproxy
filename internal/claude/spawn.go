package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Options controls a single Claude Code subprocess invocation.
type Options struct {
	// BinaryPath is the path to the `claude` binary. Empty means "claude" on PATH.
	BinaryPath string
	// Prompt is the value passed via -p.
	Prompt string
	// Workspace is the working directory the subprocess runs in.
	// Empty means "inherit ccproxy's CWD" (only sensible in tests).
	Workspace string
	// AppendSystemPrompt is passed via --append-system-prompt when non-empty.
	AppendSystemPrompt string
	// ResumeID, when non-empty, makes this a session resume via --resume.
	ResumeID string
	// Effort is passed via `--effort <level>` when non-empty.
	// Accepted values: low | medium | high | xhigh | max.
	Effort string
	// ClaudeModel is passed via `--model <name>` when non-empty.
	// Accepts aliases ("opus", "sonnet", "haiku") or full ids
	// ("claude-sonnet-4-6"). ccproxy does not validate the value —
	// claude rejects unknown ones at spawn time.
	ClaudeModel string
	// PermissionMode is passed via `--permission-mode <mode>` when non-empty.
	// Accepted: default | acceptEdits | auto | dontAsk | plan | bypassPermissions.
	// Empty = don't pass the flag (claude uses its own default, which prompts
	// — broken under -p since there is no TTY to approve on).
	PermissionMode string
	// Env is appended to os.Environ for the subprocess.
	Env []string
	// ShutdownGrace is how long to wait after SIGINT before SIGKILL.
	// Zero falls back to 5s.
	ShutdownGrace time.Duration
}

// Session is one running `claude` subprocess.
type Session struct {
	cmd       *exec.Cmd
	events    chan Event
	stderr    *ringBuffer
	waitErr   error
	waitOnce  sync.Once
	closeOnce sync.Once
	done      chan struct{}
}

const defaultShutdownGrace = 5 * time.Second

// Spawn starts a Claude Code subprocess and returns a Session whose Events
// channel is closed when the subprocess exits. Cancelling ctx triggers
// SIGINT followed by SIGKILL after Options.ShutdownGrace.
func Spawn(ctx context.Context, opts Options) (*Session, error) {
	bin := opts.BinaryPath
	if bin == "" {
		bin = "claude"
	}
	args := []string{
		"-p", opts.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if opts.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.ClaudeModel != "" {
		args = append(args, "--model", opts.ClaudeModel)
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	}

	// Note: we deliberately do not bind the subprocess to ctx via
	// exec.CommandContext, because that sends SIGKILL on cancel. We want
	// SIGINT-then-SIGKILL, handled below.
	cmd := exec.Command(bin, args...)
	if opts.Workspace != "" {
		cmd.Dir = opts.Workspace
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), opts.Env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	grace := opts.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}

	s := &Session{
		cmd:    cmd,
		events: make(chan Event, 16),
		stderr: newRingBuffer(64 << 10), // 64 KiB
		done:   make(chan struct{}),
	}

	go s.drainStderr(stderr)
	go s.readEvents(stdout)
	go s.supervise(ctx, grace)

	return s, nil
}

// Events returns the channel of parsed events. It is closed once the
// subprocess has exited and all stdout has been consumed.
func (s *Session) Events() <-chan Event { return s.events }

// Stderr returns whatever the subprocess wrote to stderr, capped at the
// ring buffer's capacity (most-recent bytes win on overflow).
func (s *Session) Stderr() string { return s.stderr.String() }

// Wait blocks until the subprocess exits and returns its exit error, if any.
func (s *Session) Wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.cmd.Wait()
		close(s.done)
	})
	<-s.done
	return s.waitErr
}

// Close requests subprocess shutdown (SIGINT, then SIGKILL after grace) and
// waits for it to exit. Safe to call multiple times.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.signal(syscall.SIGINT)
	})
	return s.Wait()
}

func (s *Session) signal(sig syscall.Signal) {
	if s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(sig)
}

func (s *Session) supervise(ctx context.Context, grace time.Duration) {
	select {
	case <-ctx.Done():
		s.closeOnce.Do(func() {
			s.signal(syscall.SIGINT)
			t := time.NewTimer(grace)
			defer t.Stop()
			select {
			case <-s.done:
			case <-t.C:
				_ = s.cmd.Process.Kill()
			}
		})
	case <-s.done:
		return
	}
}

func (s *Session) drainStderr(r io.ReadCloser) {
	defer r.Close()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.stderr.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) readEvents(r io.ReadCloser) {
	defer close(s.events)
	defer r.Close()

	br := bufio.NewReaderSize(r, 1<<20) // 1 MiB; init events can be large
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			ev, perr := parseLine(line)
			if perr != nil {
				s.events <- Event{
					Type: EventStreamError,
					Err: &StreamError{
						Message:  fmt.Sprintf("parse: %v", perr),
						Stderr:   s.stderr.String(),
						ExitCode: -1,
					},
				}
				continue
			}
			s.events <- ev
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.events <- Event{
					Type: EventStreamError,
					Err: &StreamError{
						Message:  fmt.Sprintf("read: %v", err),
						Stderr:   s.stderr.String(),
						ExitCode: -1,
					},
				}
			}
			waitErr := s.Wait()
			if waitErr != nil {
				code := -1
				var ee *exec.ExitError
				if errors.As(waitErr, &ee) {
					code = ee.ExitCode()
				}
				s.events <- Event{
					Type: EventStreamError,
					Err: &StreamError{
						Message:  fmt.Sprintf("claude exited: %v", waitErr),
						Stderr:   s.stderr.String(),
						ExitCode: code,
					},
				}
			}
			return
		}
	}
}

// readLine reads one NDJSON line, stripping the trailing newline. It tolerates
// arbitrarily long lines (the init system event in particular is huge).
func readLine(br *bufio.Reader) ([]byte, error) {
	var full []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 {
			if full == nil && !isPrefix {
				return chunk, err
			}
			full = append(full, chunk...)
		}
		if !isPrefix {
			return full, err
		}
		if err != nil {
			return full, err
		}
	}
}
