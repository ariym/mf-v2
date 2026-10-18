// Package process implements local daemon control for macOS and Linux.
package process

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/google/uuid"
)

type State struct {
	PID      int    `json:"pid"`
	Instance string `json:"instance"`
	Ready    bool   `json:"ready"`
}
type Lock struct {
	file *os.File
	Dir  string
}

func Prepare(dir string) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	st, e := os.Lstat(dir)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state directory must be a real directory: %s", dir)
	}
	return os.Chmod(dir, 0700)
}
func Acquire(dir string) (*Lock, error) {
	if e := Prepare(dir); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("server already running for this database: %w", e)
	}
	return &Lock{f, dir}, nil
}
func (l *Lock) Close()          { _ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); _ = l.file.Close() }
func Socket(dir string) string  { return filepath.Join(dir, "control.sock") }
func LogPath(dir string) string { return filepath.Join(dir, "daemon.log") }
func ControlListener(dir string) (net.Listener, error) {
	path := Socket(dir)
	if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	listener, e := net.Listen("unix", path)
	if e != nil {
		return nil, fmt.Errorf("create local control socket %s: %w", path, e)
	}
	if e = os.Chmod(path, 0600); e != nil {
		listener.Close()
		return nil, e
	}
	return listener, nil
}
func controlClient(dir string) *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", Socket(dir))
	}}}
}
func Control(ctx context.Context, dir, method, path string, out any) error {
	c := controlClient(dir)
	defer c.CloseIdleConnections()
	req, e := http.NewRequestWithContext(ctx, method, "http://local"+path, nil)
	if e != nil {
		return e
	}
	resp, e := c.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		if e = json.NewDecoder(resp.Body).Decode(&body); e != nil {
			return fmt.Errorf("control: HTTP %d", resp.StatusCode)
		}
		return errors.New(body.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
func WriteState(dir string, s State) error {
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, "pid.json"), b, 0600)
}
func Start(ctx context.Context, c config.Config, out io.Writer) error {
	if e := Prepare(c.StateDir()); e != nil {
		return e
	}
	// Early feedback only; the child owns the authoritative lock throughout startup.
	lock, e := Acquire(c.StateDir())
	if e != nil {
		return e
	}
	lock.Close()
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	instance := uuid.NewString()
	f, e := os.OpenFile(LogPath(c.StateDir()), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	logInfo, e := f.Stat()
	if e != nil {
		return e
	}
	startupError := func(reason string) error {
		log, err := os.Open(LogPath(c.StateDir()))
		if err != nil {
			return fmt.Errorf("%s; inspect mf daemon logs", reason)
		}
		defer log.Close()
		info, err := log.Stat()
		if err != nil {
			return fmt.Errorf("%s; inspect mf daemon logs", reason)
		}
		start := max(logInfo.Size(), info.Size()-(32<<10))
		detail, _ := io.ReadAll(io.NewSectionReader(log, start, info.Size()-start))
		return fmt.Errorf("%s; inspect mf daemon logs\n%s", reason, strings.TrimSpace(string(detail)))
	}
	cmd := exec.Command(executable, "--config", c.Path, "_daemon-run", "--instance", instance)
	cmd.Dir = filepath.Dir(c.Path)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	cleanup := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	for {
		select {
		case e := <-done:
			return startupError(fmt.Sprintf("daemon failed before readiness (%v)", e))
		case <-ctx.Done():
			cleanup()
			return ctx.Err()
		case <-timer.C:
			cleanup()
			return startupError("daemon startup timed out")
		case <-ticker.C:
			var status State
			if Control(ctx, c.StateDir(), "GET", "/status", &status) == nil && status.Instance == instance && status.Ready {
				fmt.Fprintf(out, "Daemon started (pid %d) at http://%s\n", status.PID, c.Address())
				return nil
			}
		}
	}
}
func Stop(ctx context.Context, c config.Config, force bool, out io.Writer) error {
	var before State
	if e := Control(ctx, c.StateDir(), "GET", "/status", &before); e != nil {
		return fmt.Errorf("daemon is not reachable at %s: %w", Socket(c.StateDir()), e)
	}
	path := "/stop"
	if force {
		path += "?force=true"
	}
	if e := Control(ctx, c.StateDir(), "POST", path, nil); e != nil {
		return e
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("daemon shutdown timed out; inspect mf daemon logs")
		case <-ticker.C:
			lock, e := Acquire(c.StateDir())
			if e == nil {
				lock.Close()
				fmt.Fprintln(out, "Daemon stopped")
				return nil
			}
			var current State
			if Control(ctx, c.StateDir(), "GET", "/status", &current) == nil && current.Instance != before.Instance {
				return fmt.Errorf("original daemon stopped but another server now owns the database")
			}
		}
	}
}
func Logs(ctx context.Context, dir string, follow bool, out io.Writer) error {
	f, e := os.Open(LogPath(dir))
	if os.IsNotExist(e) {
		return fmt.Errorf("no daemon logs found at %s", LogPath(dir))
	}
	if e != nil {
		return e
	}
	defer f.Close()
	// Seek backwards in blocks so a large log does not have to be read in full.
	st, e := f.Stat()
	if e != nil {
		return e
	}
	position := st.Size()
	var tail []byte
	for position > 0 && strings.Count(string(tail), "\n") <= 20 {
		size := min(int64(8192), position)
		position -= size
		block := make([]byte, size)
		if _, e = f.ReadAt(block, position); e != nil {
			return e
		}
		tail = append(block, tail...)
	}
	text := strings.TrimSuffix(string(tail), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	if len(tail) > 0 {
		if _, e = fmt.Fprintln(out, strings.Join(lines, "\n")); e != nil {
			return e
		}
	}
	if !follow {
		return nil
	}
	if _, e = f.Seek(st.Size(), io.SeekStart); e != nil {
		return e
	}
	reader := bufio.NewReader(f)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		line, e := reader.ReadString('\n')
		if line != "" {
			if _, we := io.WriteString(out, line); we != nil {
				return we
			}
		}
		if e != nil && e != io.EOF {
			return e
		}
		if e == io.EOF {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
}
