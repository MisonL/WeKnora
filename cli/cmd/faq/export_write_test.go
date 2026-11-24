package faq

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A snapshot rewrites the caller's backup file, so the file it leaves behind
// must be readable the way an ordinary write would have made it -- not the
// 0600 a temporary file opens with -- and its directory entry has to be on
// disk when the rename returns.
func TestWriteFileAtomicLeavesReadableFileAndPersistsTheName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix mode bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(path, []byte(`[{"standard_question":"q"}]`)); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != `[{"standard_question":"q"}]` {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o044 == 0 {
		t.Fatalf("mode = %v, want group/other read (a backup replacement)", mode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("leftover %q in %s, want only the snapshot", e.Name(), dir)
		}
	}
}
