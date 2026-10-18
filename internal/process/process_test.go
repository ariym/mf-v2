package process

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogTailAndLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	lock, e := Acquire(dir)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := Acquire(dir); e == nil {
		second.Close()
		t.Fatal("acquired duplicate lock")
	}
	lock.Close()
	lock, e = Acquire(dir)
	if e != nil {
		t.Fatal(e)
	}
	lock.Close()
	var content strings.Builder
	for n := 1; n <= 30; n++ {
		fmt.Fprintf(&content, "line %d\n", n)
	}
	if e = os.WriteFile(LogPath(dir), []byte(content.String()), 0600); e != nil {
		t.Fatal(e)
	}
	var output bytes.Buffer
	if e = Logs(context.Background(), dir, false, &output); e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 20 || lines[0] != "line 11" || lines[19] != "line 30" {
		t.Fatalf("tail: %q", output.String())
	}
}
