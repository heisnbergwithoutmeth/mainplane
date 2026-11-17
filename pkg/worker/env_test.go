package worker

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// skipIfNoBash skips the test when bash is not on PATH or the platform is
// windows: env.go's bash interpreter is a unix login shell, and the worker's
// windows interpreter is pwsh (not exercised here).
func skipIfNoBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash interpreter tests run on unix; windows uses pwsh")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
}

// newBashEnv starts a bash environment as the worker's own user and arranges
// for it to be killed when the test ends.
func newBashEnv(t *testing.T) *env {
	t.Helper()
	e, err := start(interps["bash"], nil, "test-session", "test-key", "http://test")
	if err != nil {
		t.Fatalf("start bash: %v", err)
	}
	t.Cleanup(e.kill)
	return e
}

// TestRunLargeCodeEarlyLargeOutput reproduces the deadlock: a script larger
// than the ~64KiB pipe buffer whose first command emits more than ~64KiB to
// stdout. With a single blocking stdin write before the read loop, the
// interpreter fills the stdout pipe and stops reading stdin, so the worker
// blocks writing it and nothing drains either end. With the fix, stdin is
// written while stdout is drained and the run completes with its output.
func TestRunLargeCodeEarlyLargeOutput(t *testing.T) {
	skipIfNoBash(t)
	e := newBashEnv(t)

	// ~400KiB of comments makes the code well over the 64KiB pipe buffer.
	padding := strings.Repeat("# padding line\n", 32000)
	// The first command emits ~268KiB of base64 to stdout, over the buffer.
	code := "head -c 200000 /dev/urandom | base64\n" + padding

	var total atomic.Int64
	emit := func(b []byte) error {
		total.Add(int64(len(b)))
		return nil
	}

	// A generous ctx so a real run completes; the watchdog ends the test
	// fast if the run deadlocks, instead of waiting the whole ctx out.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type result struct {
		exit    int
		alive   bool
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		exit, alive, err := e.run(ctx, code, "mp-done-heavy", emit)
		done <- result{exit, alive, err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("run: %v", r.err)
		}
		if !r.alive {
			t.Fatalf("env died: exit=%d elapsed=%s (deadlock: the run was killed instead of completing)", r.exit, r.elapsed)
		}
		if r.exit != 0 {
			t.Fatalf("exit=%d, want 0", r.exit)
		}
		if got := total.Load(); got < 200000 {
			t.Fatalf("streamed %d bytes, want >= 200000 (early stdout was not drained)", got)
		}
	case <-time.After(5 * time.Second):
		e.kill()
		t.Fatalf("run deadlocked: did not complete in 5s")
	}
}

// TestRunPersistentEnv verifies the interpreter persists between runs: a
// variable set in one run is visible in the next. This guards against a fix
// that closes stdin after writing, which would end the interpreter and drop
// the variables, cwd, and jobs a persistent environment is there to keep.
func TestRunPersistentEnv(t *testing.T) {
	skipIfNoBash(t)
	e := newBashEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if exit, alive, err := e.run(ctx, "X=42\n", "mp-done-persist-1", func([]byte) error { return nil }); err != nil || exit != 0 || !alive {
		t.Fatalf("first run: exit=%d alive=%v err=%v", exit, alive, err)
	}
	var mu sync.Mutex
	var out bytes.Buffer
	emit := func(b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		out.Write(b)
		return nil
	}
	if exit, alive, err := e.run(ctx, "echo $X\n", "mp-done-persist-2", emit); err != nil || exit != 0 || !alive {
		t.Fatalf("second run: exit=%d alive=%v err=%v", exit, alive, err)
	}
	mu.Lock()
	got := out.String()
	mu.Unlock()
	if !strings.Contains(got, "42") {
		t.Fatalf("output=%q, want 42 (variable did not persist; was stdin closed?)", got)
	}
}

// TestRunTimeoutStillKills verifies a run past the context deadline is killed
// and returns the deadline error, so the timeout path (and the env reset that
// serve.go keys off ctx.Err) still works under the fix.
func TestRunTimeoutStillKills(t *testing.T) {
	skipIfNoBash(t)
	e := newBashEnv(t)

	emit := func(b []byte) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, alive, err := e.run(ctx, "sleep 30\n", "mp-done-timeout", emit)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	if alive {
		t.Fatalf("alive=true, want false (env should be killed on timeout)")
	}
}

// TestRunBackgroundLargeCodeEarlyLargeOutput reproduces the deadlock on the
// background-job path, which calls run with context.Background and no deadline.
// Without the fix the run hangs forever and only a session Kill ends it; with
// the fix it completes normally with its output streamed.
func TestRunBackgroundLargeCodeEarlyLargeOutput(t *testing.T) {
	skipIfNoBash(t)
	e := newBashEnv(t)

	padding := strings.Repeat("# padding line\n", 32000)
	code := "head -c 200000 /dev/urandom | base64\n" + padding

	var total atomic.Int64
	emit := func(b []byte) error {
		total.Add(int64(len(b)))
		return nil
	}

	type result struct {
		exit  int
		alive bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		exit, alive, err := e.run(context.Background(), code, "mp-done-bg", emit)
		done <- result{exit, alive, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("run: %v", r.err)
		}
		if r.exit != 0 {
			t.Fatalf("exit=%d, want 0", r.exit)
		}
		if !r.alive {
			t.Fatalf("alive=false, want true (env should keep running on the job path)")
		}
		if got := total.Load(); got < 200000 {
			t.Fatalf("streamed %d bytes, want >= 200000", got)
		}
	case <-time.After(10 * time.Second):
		e.kill()
		t.Fatalf("run deadlocked: background job did not complete in 10s")
	}
}
