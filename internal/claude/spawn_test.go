package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeClaudeOpts builds Options pointing at the test binary running in
// helper-process mode. Mode names map to canned NDJSON in TestHelperProcess.
func fakeClaudeOpts(t *testing.T, mode string) Options {
	t.Helper()
	return Options{
		BinaryPath: os.Args[0],
		Prompt:     "ignored",
		Env: []string{
			"CCPROXY_HELPER=1",
			"CCPROXY_HELPER_MODE=" + mode,
		},
		ShutdownGrace: 200 * time.Millisecond,
	}
}

// TestMain re-invokes this test binary as a fake `claude` when the env var
// is set. The fake ignores the args ccproxy would normally pass and emits
// canned events based on CCPROXY_HELPER_MODE.
func TestMain(m *testing.M) {
	if os.Getenv("CCPROXY_HELPER") == "1" {
		runFakeClaude()
		return
	}
	os.Exit(m.Run())
}

func runFakeClaude() {
	mode := os.Getenv("CCPROXY_HELPER_MODE")
	switch mode {
	case "happy":
		fmt.Println(`{"type":"system","subtype":"init","cwd":"/tmp","model":"claude-opus-4-7","tools":["Bash"],"session_id":"sess-1"}`)
		fmt.Println(`{"type":"assistant","message":{"id":"msg-1","model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"Hello world"}],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":2}},"session_id":"sess-1"}`)
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"duration_ms":42,"num_turns":1,"result":"Hello world","stop_reason":"end_turn","total_cost_usd":0.001,"session_id":"sess-1","usage":{"input_tokens":3,"output_tokens":2}}`)
		os.Exit(0)
	case "garbage":
		fmt.Println(`{"type":"system","subtype":"init","session_id":"sess-2"}`)
		fmt.Println(`not json at all`)
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":0,"result":"","stop_reason":"end_turn","session_id":"sess-2"}`)
		os.Exit(0)
	case "stderr-then-fail":
		fmt.Fprintln(os.Stderr, "boom: something went wrong")
		os.Exit(7)
	case "hang":
		// Block forever — used to test cancellation.
		select {}
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

func collect(t *testing.T, sess *Session) []Event {
	t.Helper()
	var out []Event
	for ev := range sess.Events() {
		out = append(out, ev)
	}
	return out
}

func TestSpawn_HappyPath(t *testing.T) {
	sess, err := Spawn(t.Context(), fakeClaudeOpts(t, "happy"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := collect(t, sess)

	if got, want := len(events), 3; got != want {
		t.Fatalf("event count = %d, want %d (events: %+v)", got, want, events)
	}
	if events[0].Type != EventSystem || events[0].System.Subtype != "init" {
		t.Errorf("event[0] = %+v, want system/init", events[0])
	}
	if events[1].Type != EventAssistant {
		t.Fatalf("event[1] type = %q, want assistant", events[1].Type)
	}
	if got := events[1].Assistant.Message.Content[0].Text; got != "Hello world" {
		t.Errorf("assistant text = %q, want %q", got, "Hello world")
	}
	if events[2].Type != EventResult {
		t.Fatalf("event[2] type = %q, want result", events[2].Type)
	}
	if events[2].Result.Result != "Hello world" {
		t.Errorf("result.result = %q", events[2].Result.Result)
	}
	if err := sess.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestSpawn_GarbageLineBecomesStreamError(t *testing.T) {
	sess, err := Spawn(t.Context(), fakeClaudeOpts(t, "garbage"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := collect(t, sess)

	var sawError bool
	for _, ev := range events {
		if ev.Type == EventStreamError {
			sawError = true
			if !strings.Contains(ev.Err.Message, "parse") {
				t.Errorf("stream error message = %q, want it to mention parse", ev.Err.Message)
			}
		}
	}
	if !sawError {
		t.Errorf("expected a stream_error event, got %+v", events)
	}
}

func TestSpawn_NonZeroExitSurfacesStderr(t *testing.T) {
	sess, err := Spawn(t.Context(), fakeClaudeOpts(t, "stderr-then-fail"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	events := collect(t, sess)

	if len(events) == 0 {
		t.Fatal("expected at least a stream_error event")
	}
	last := events[len(events)-1]
	if last.Type != EventStreamError {
		t.Fatalf("last event = %+v, want stream_error", last)
	}
	if last.Err.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", last.Err.ExitCode)
	}
	if !strings.Contains(last.Err.Stderr, "boom") {
		t.Errorf("stderr capture = %q, want it to contain 'boom'", last.Err.Stderr)
	}
}

func TestSpawn_ContextCancelKillsHangingProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	sess, err := Spawn(ctx, fakeClaudeOpts(t, "hang"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Give the subprocess a beat to actually be in select{}, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-sess.Events():
			if !ok {
				if !exitedAbnormally(sess) {
					t.Errorf("expected abnormal exit after cancel, got Wait=%v", sess.Wait())
				}
				return
			}
		case <-deadline:
			t.Fatal("subprocess did not exit within 2s of cancellation")
		}
	}
}

func exitedAbnormally(s *Session) bool {
	err := s.Wait()
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	return errors.As(err, &ee)
}
