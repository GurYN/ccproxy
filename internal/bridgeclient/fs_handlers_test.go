package bridgeclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/guryn/ccproxy/internal/bridge"
)

func newHandlers(t *testing.T) (*FSHandlers, string) {
	t.Helper()
	root := t.TempDir()
	return NewFSHandlers(root), root
}

func TestFS_StatReadDirRead(t *testing.T) {
	h, root := newHandlers(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, rerr := h.Stat(context.Background(), bridge.StatParams{Path: "a.txt"})
	if rerr != nil {
		t.Fatalf("stat: %v", rerr)
	}
	if st.Size != 5 || st.IsDir {
		t.Errorf("unexpected stat: %+v", st)
	}

	rd, rerr := h.ReadDir(context.Background(), bridge.ReadDirParams{Path: "."})
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	if len(rd.Entries) != 1 || rd.Entries[0].Name != "a.txt" {
		t.Errorf("unexpected entries: %+v", rd.Entries)
	}

	r, rerr := h.Read(context.Background(), bridge.ReadParams{Path: "a.txt", Length: 1024})
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if string(r.Data) != "hello" || !r.EOF {
		t.Errorf("unexpected read: %+v", r)
	}
}

func TestFS_WriteCreateRemove(t *testing.T) {
	h, _ := newHandlers(t)
	if rerr := h.Create(context.Background(), bridge.CreateParams{Path: "new.txt"}); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	w, rerr := h.Write(context.Background(), bridge.WriteParams{Path: "new.txt", Data: []byte("world"), Truncate: true})
	if rerr != nil {
		t.Fatalf("write: %v", rerr)
	}
	if w.Written != 5 {
		t.Errorf("written = %d", w.Written)
	}
	if rerr := h.Remove(context.Background(), bridge.RemoveParams{Path: "new.txt"}); rerr != nil {
		t.Fatalf("remove: %v", rerr)
	}
	if _, rerr := h.Stat(context.Background(), bridge.StatParams{Path: "new.txt"}); rerr == nil || rerr.Code != bridge.ErrCodeNotFound {
		t.Errorf("expected not_found after remove, got %+v", rerr)
	}
}

func TestFS_RejectsEscape(t *testing.T) {
	h, _ := newHandlers(t)
	cases := []string{"../etc/passwd", "/etc/passwd", "a/../../../etc"}
	for _, c := range cases {
		if _, rerr := h.Stat(context.Background(), bridge.StatParams{Path: c}); rerr == nil {
			t.Errorf("expected error for path %q", c)
		}
	}
}

func TestFS_TruncateToSize(t *testing.T) {
	h, root := newHandlers(t)
	// Existing 11-byte file.
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Truncate to 0 (zero out).
	if _, rerr := h.Write(context.Background(), bridge.WriteParams{Path: "x.txt", Offset: 0, Data: nil, Truncate: true}); rerr != nil {
		t.Fatalf("truncate(0): %v", rerr)
	}
	st, _ := h.Stat(context.Background(), bridge.StatParams{Path: "x.txt"})
	if st.Size != 0 {
		t.Errorf("after truncate(0), size = %d, want 0", st.Size)
	}

	// Truncate to N > 0 must NOT zero the file (the bug we just fixed).
	_ = os.WriteFile(filepath.Join(root, "y.txt"), []byte("hello world"), 0o644)
	if _, rerr := h.Write(context.Background(), bridge.WriteParams{Path: "y.txt", Offset: 5, Data: nil, Truncate: true}); rerr != nil {
		t.Fatalf("truncate(5): %v", rerr)
	}
	st, _ = h.Stat(context.Background(), bridge.StatParams{Path: "y.txt"})
	if st.Size != 5 {
		t.Errorf("after truncate(5), size = %d, want 5", st.Size)
	}
	body, _ := os.ReadFile(filepath.Join(root, "y.txt"))
	if string(body) != "hello" {
		t.Errorf("after truncate(5), content = %q, want %q", body, "hello")
	}
}

func TestFS_WriteThenTruncatePreservesContent(t *testing.T) {
	// Reproduces the user-reported "file empty after edit" bug:
	// 1. Truncate(0), 2. Write(buf, 0), 3. Truncate(len(buf)).
	// Step 3 is what Claude's writer might issue at close to set the
	// exact final size; the buggy implementation reset it to 0.
	h, root := newHandlers(t)
	_ = os.WriteFile(filepath.Join(root, "f.txt"), []byte("old content"), 0o644)
	ctx := context.Background()

	if _, rerr := h.Write(ctx, bridge.WriteParams{Path: "f.txt", Offset: 0, Truncate: true}); rerr != nil {
		t.Fatalf("step 1: %v", rerr)
	}
	if _, rerr := h.Write(ctx, bridge.WriteParams{Path: "f.txt", Offset: 0, Data: []byte("hi there")}); rerr != nil {
		t.Fatalf("step 2: %v", rerr)
	}
	if _, rerr := h.Write(ctx, bridge.WriteParams{Path: "f.txt", Offset: 8, Truncate: true}); rerr != nil {
		t.Fatalf("step 3: %v", rerr)
	}
	body, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	if string(body) != "hi there" {
		t.Errorf("got %q, want %q", body, "hi there")
	}
}

func TestFS_RemoveRoot(t *testing.T) {
	h, _ := newHandlers(t)
	if rerr := h.Remove(context.Background(), bridge.RemoveParams{Path: "."}); rerr == nil {
		t.Errorf("must refuse to remove root")
	}
}
