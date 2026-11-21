package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os/exec"
	"strconv"
	"testing"
)

// runJob runs code as a background job on a fresh *server over a pipe and
// returns its Result frame, draining the Started frame the worker sends first.
func runJob(t *testing.T, code string) Frame {
	t.Helper()
	near, far := net.Pipe()
	t.Cleanup(func() { _ = near.Close(); _ = far.Close() })
	s := &server{Local: Local{Scratch: t.TempDir(), Interps: []string{"bash"}}, conn: near, envs: map[string]*env{}, dead: map[string]bool{}}
	go func() {
		if _, err := Decode(bufio.NewReader(far)); err != nil {
			t.Errorf("read started: %v", err)
		}
	}()
	f, err := s.job(Frame{Header: Header{Kind: Run, Job: "j1", Session: "s1"}, Body: []byte(code)}, interps["bash"])
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	return f
}

// TestJobTailClampCounts: a job whose output runs past twice the byte limit
// ships a tail whose front is dropped at line boundaries, and the Result
// frame's Cut counts the lines that left. The harness adds Cut to its own
// clip and announces the total, so the count must be exact end to end.
func TestJobTailClampCounts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// 12000 lines of "abcdefgh\n" (9 bytes each) is 108000 bytes, just over
	// 2*MaxBytes. The clamp keeps the last MaxBytes rounded down to whole
	// lines: floor(51200/9) = 5688 lines, and counts the 6312 it dropped.
	const n, kept, cut = 12000, 5688, 6312
	result := runJob(t, "yes abcdefgh | head -n "+strconv.Itoa(n))
	if result.Exit != 0 {
		t.Errorf("exit: got %d, want 0", result.Exit)
	}
	if result.Cut != cut {
		t.Errorf("Cut: got %d, want %d", result.Cut, cut)
	}
	if lines := bytes.Count(result.Body, []byte("\n")); lines != kept {
		t.Errorf("tail lines: got %d, want %d", lines, kept)
	}
	if result.Path == "" {
		t.Error("Result has no log path")
	}
}

// TestJobGiantLineCut: one line longer than 2*MaxBytes, with no newline. The
// clamp's mid flag flips (the kept tail begins mid-line), and the cut counts
// the split line as one. Guards the mid branch separately from the all-short
// lines case above, since it is the subtle part of the clamp.
func TestJobGiantLineCut(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	result := runJob(t, "head -c 150000 /dev/zero | tr '\\0' x")
	if result.Exit != 0 {
		t.Errorf("exit: got %d, want 0", result.Exit)
	}
	if result.Cut != 1 {
		t.Errorf("Cut: got %d, want 1 (one split line)", result.Cut)
	}
	if len(result.Body) < MaxBytes || len(result.Body) >= 2*MaxBytes {
		t.Errorf("body len: got %d, want in [%d, %d)", len(result.Body), MaxBytes, 2*MaxBytes)
	}
	if bytes.Contains(result.Body, []byte("\n")) {
		t.Errorf("body should have no newline: %q", result.Body[:min(len(result.Body), 40)])
	}
}

// TestCutWireCompat: the Cut field is omitempty, so an older worker that does
// not send it produces wire bytes with no "cut" key (unchanged shape), and an
// older harness whose Header has no Cut field ignores an incoming "cut"
// without error (json.Unmarshal skips unknown fields).
func TestCutWireCompat(t *testing.T) {
	b, err := json.Marshal(Header{Kind: Result, Exit: 0, Cut: 0})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("cut")) {
		t.Errorf("Cut=0 marshals as %s, want no cut key", b)
	}
	b, err = json.Marshal(Header{Kind: Result, Exit: 0, Cut: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"cut":5`)) {
		t.Errorf("Cut=5 marshals as %s, want cut:5", b)
	}
	type oldHeader struct {
		Kind string `json:"kind"`
		Exit int    `json:"exit,omitempty"`
	}
	var old oldHeader
	if err := json.Unmarshal([]byte(`{"kind":"result","exit":0,"cut":7}`), &old); err != nil {
		t.Fatalf("old harness unmarshal: %v", err)
	}
	if old.Kind != "result" {
		t.Errorf("old harness kind: %q", old.Kind)
	}
}
