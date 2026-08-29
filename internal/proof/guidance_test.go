package proof

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotGuidanceHashesAndSorts(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")
	if err := os.WriteFile(a, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("bravo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := SnapshotGuidance([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != a || got[0].SHA256 == "" || got[0].Bytes != 6 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestSnapshotGuidanceRejectsMissingInvalidAndOversized(t *testing.T) {
	if _, err := SnapshotGuidance([]string{"relative.md"}); err == nil {
		t.Fatal("relative guidance accepted")
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotGuidance([]string{empty}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty error = %v", err)
	}
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotGuidance([]string{bad}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid error = %v", err)
	}
}
