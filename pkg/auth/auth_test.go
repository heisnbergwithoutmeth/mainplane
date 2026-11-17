package auth

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// childEnv names the store a test presents to a child of itself. A
// cross-process test runs this binary with childEnv set to an auth.json
// path and a verb in os.Args, exercising the same Store an install's
// root CLI and the live harness share: this is the only way to put two
// real processes against one table from a Go test.
const childEnv = "MAINPLANE_AUTH_CHILD"

// TestMain is also a child of a cross-process test: with childEnv set it
// runs the verb in os.Args against the store at childEnv, and exits 0 on
// success or 1 on any error; otherwise it runs the tests.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		os.Exit(childMain())
	}
	os.Exit(m.Run())
}

// childMain is the body of a child process. It returns the exit code.
func childMain() int {
	store := Store{Path: os.Getenv(childEnv)}
	switch os.Args[1] {
	case "issue-burst":
		// Issue n keys, each prefixed, in one process as fast as it can.
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		prefix := os.Args[3]
		for i := range n {
			if _, err := store.Issue(Key, fmt.Sprintf("%s%04d", prefix, i)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
	case "hold-lock":
		// Hold the cross-process lock exclusive for secs, so a test in the
		// parent proves a Load waits for it instead of seeing a torn file.
		secs, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		f, err := os.OpenFile(store.Path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = f.Close() }()
		if err := lockFile(f, store.Path, true); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("locked")
		time.Sleep(time.Duration(secs) * time.Second)
	default:
		fmt.Fprintln(os.Stderr, "unknown verb", os.Args[1])
		return 1
	}
	return 0
}

// execChild runs this test binary as a child process against the store at
// path with verb and args, returning the child's combined output and error.
func execChild(t *testing.T, path, verb string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{verb}, args...)...)
	cmd.Env = append(os.Environ(), childEnv+"="+path)
	return cmd.CombinedOutput()
}

// newStore makes a Store at a fresh path in a temp dir.
func newStore(t *testing.T) (Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	return Store{Path: path}, path
}

// authJSONCorrupt wraps err with the file's contents so a corruption-caught
// failure shows the bytes that did not parse.
func authJSONCorrupt(err error, path string) error {
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("%w (and reading the file: %w)", err, readErr)
	}
	return fmt.Errorf("%w; auth.json contents: %q", err, b)
}

// head is the first n of s, for short failure messages.
func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// TestConcurrentIssueAcrossProcessesLosesNoEntry is the cross-process half
// of the bug: the running harness (POST /keys) and the root CLI (key new)
// share one auth.json. Several child processes Issue at once and every entry
// lands; the in-process writes RWMutex cannot reach across a process
// boundary, so without the cross-process lock this loses entries and
// destroys the file.
func TestConcurrentIssueAcrossProcessesLosesNoEntry(t *testing.T) {
	store, path := newStore(t)
	const procs, perProc = 8, 25
	var wg sync.WaitGroup
	for p := range procs {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			out, err := execChild(t, path, "issue-burst", strconv.Itoa(perProc), fmt.Sprintf("p%d-", p))
			if err != nil {
				t.Errorf("issue-burst p%d: %v: %s", p, err, out)
			}
		}(p)
	}
	wg.Wait()

	tab, err := store.Load()
	if err != nil {
		t.Fatalf("Load after concurrent cross-process Issues: %v", authJSONCorrupt(err, path))
	}
	have := map[string]bool{}
	for _, e := range tab[Key] {
		have[e.Name] = true
	}
	var missing []string
	for p := range procs {
		for i := range perProc {
			name := fmt.Sprintf("p%d-%04d", p, i)
			if !have[name] {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d entries lost across processes (first few: %v)", len(missing), procs*perProc, head(missing, 5))
	}
}

// TestRevokeSurvivesConcurrentIssueAcrossProcesses is the reported exploit: a
// revoked key stays revoked even while concurrent Issue calls (the harness's
// POST /keys) race the operator's revoke. Without the cross-process lock, a
// concurrent Issue's stale snapshot rewrites the table after the revoke and
// the entry comes back.
func TestRevokeSurvivesConcurrentIssueAcrossProcesses(t *testing.T) {
	const iters = 5
	const procs, perProc = 4, 25
	for iter := range iters {
		store, path := newStore(t)
		target, err := store.Issue(Key, "target")
		if err != nil {
			t.Fatalf("Issue target: %v", err)
		}
		if !store.Check(Key, target) {
			t.Fatal("target does not authenticate before the race")
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Revoke(Key, "target"); err != nil {
				t.Errorf("Revoke target: %v", err)
			}
		}()
		for p := range procs {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				out, err := execChild(t, path, "issue-burst", strconv.Itoa(perProc), fmt.Sprintf("c%d-", p))
				if err != nil {
					t.Errorf("issue-burst c%d: %v: %s", p, err, out)
				}
			}(p)
		}
		wg.Wait()

		if store.Check(Key, target) {
			t.Fatalf("target still authenticates: a concurrent Issue undid the revoke (iter %d)", iter)
		}
		// And the table is whole, not corrupted.
		if _, err := store.Load(); err != nil {
			t.Fatalf("Load after the race: %v", authJSONCorrupt(err, path))
		}
	}
}

// TestTheFileStaysValidJSONUnderConcurrentCrossProcessWriters checks the
// permanent-corruption half of the bug: with truncate-then-write and no
// cross-process lock, two short writes after a longer one leave trailing
// bytes that turn the file into invalid JSON forever. With the lock every
// reader sees the old table or the new one, never half of one.
func TestTheFileStaysValidJSONUnderConcurrentCrossProcessWriters(t *testing.T) {
	store, path := newStore(t)
	if _, err := store.Issue(Key, "seed"); err != nil {
		t.Fatalf("seed Issue: %v", err)
	}

	const procs, perProc = 8, 25
	var wg sync.WaitGroup
	for p := range procs {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			out, err := execChild(t, path, "issue-burst", strconv.Itoa(perProc), fmt.Sprintf("j%d-", p))
			if err != nil {
				t.Errorf("issue-burst j%d: %v: %s", p, err, out)
			}
		}(p)
	}
	// Concurrent Load in this process: each must see a valid table.
	var loads, bad atomic.Int64
	for range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if _, err := store.Load(); err != nil {
					bad.Add(1)
				} else {
					loads.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := bad.Load(); got != 0 {
		t.Errorf("%d of %d Loads saw a torn or otherwise unreadable table", got, got+loads.Load())
	}
	// After all writes, the file on disk must parse as JSON.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var probe Table
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("final auth.json is invalid JSON (%v); contents: %q", err, b)
	}
}

// TestLoadBlocksOnAnotherProcessExclusiveLock is the cross-process exclusion
// proof: a child holds the lock exclusive, and a Load in this process does
// not return until the child releases it. Without acquire's shared lock a
// Load could read the file mid-write; here it waits.
func TestLoadBlocksOnAnotherProcessExclusiveLock(t *testing.T) {
	store, path := newStore(t)
	if _, err := store.Issue(Key, "seed"); err != nil {
		t.Fatalf("seed Issue: %v", err)
	}

	cmd := exec.Command(os.Args[0], "hold-lock", "2")
	cmd.Env = append(os.Environ(), childEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	// Wait for the child to print "locked" so the held lock is in hand
	// before this process tries to load.
	r := bufio.NewScanner(stdout)
	if !r.Scan() || r.Text() != "locked" {
		t.Fatalf("child did not lock: %q", r.Text())
	}
	start := time.Now()
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load after held lock: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("Load returned in %v while another process held the exclusive lock; the cross-process shared lock did not block", elapsed)
	}
}
