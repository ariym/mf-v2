package executable

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCancellationTerminatesGrandchildren(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	leak := filepath.Join(dir, "leak")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// If only the outer shell dies, its child will create leak after one second.
	cmd := exec.CommandContext(ctx, "sh", "-c", `sh -c 'echo ready > "$1"; sleep 1; echo leaked > "$2"' child "$1" "$2" & wait`, "parent", ready, leak)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cmd) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e := os.Stat(ready); e == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("expected cancellation: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess did not stop")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, e := os.Stat(leak); !os.IsNotExist(e) {
		t.Fatal("grandchild escaped cancellation")
	}
}
