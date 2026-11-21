package harness

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// countLines is the number of newline-separated lines in s: the count of
// newlines, plus one when the last line has no trailing newline.
func countLines(s string) int {
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// jobCase is one background-job truncation scenario for TestJobTruncation.
type jobCase struct {
	name     string
	body     []byte // the log tail the worker ships as the job's Result body
	cut      int    // the lines the worker's tail clamp dropped before the body
	wantCut  string // the cut notice the message must contain, "" for none
	wantTail int    // lines the kept tail must have
}

// jobStepFunc is the test's model: the first step starts a background run, and
// any later step ends. sawJob closes once a step's request contains the job
// completion message, so the test can wait for the path that used to drop it.
func jobStepFunc() (func(string) turn, <-chan struct{}) {
	sawJob := make(chan struct{}, 1)
	seen := 0
	return func(body string) turn {
		seen++
		if seen == 1 {
			return turn{code: "ignored", bg: true, input: 10}
		}
		if strings.Contains(body, "[job ") {
			select {
			case sawJob <- struct{}{}:
			default:
			}
		}
		return turn{input: 10}
	}, sawJob
}

// findJobMessage returns the body of the job completion Message in the session,
// or empty if there is none.
func findJobMessage(r *rig, session string) string {
	for _, m := range r.bodies(session, statefile.Message, "") {
		if strings.HasPrefix(m, "[job ") {
			return m
		}
	}
	return ""
}

// assertJobMessage checks the posted job message: it names the exit code and
// the log path, and announces the cut the way finish does for a run, or carries
// no notice when nothing was cut. The tail keeps the expected line count.
func assertJobMessage(t *testing.T, job, wantCut string, wantTail int) {
	t.Helper()
	if !strings.Contains(job, "exited 0") {
		t.Errorf("job message missing exit code: %q", job)
	}
	if !strings.Contains(job, "Log: /scratch/jobs/") {
		t.Errorf("job message missing log path: %q", job)
	}
	if wantCut == "" {
		if strings.Contains(job, "lines cut") {
			t.Errorf("job message should carry no cut notice: %q", job)
		}
	} else if !strings.Contains(job, wantCut) {
		t.Errorf("job message missing %q: %s", wantCut, job)
	}
	_, tail, _ := strings.Cut(job, "\n")
	if got := countLines(tail); got != wantTail {
		t.Errorf("tail lines: got %d, want %d", got, wantTail)
	}
}

func runJobCase(t *testing.T, tc jobCase) {
	t.Helper()
	r := newRig(t)
	r.mu.Lock()
	r.jobBody, r.jobCut, r.jobExit = tc.body, tc.cut, 0
	r.mu.Unlock()
	step, sawJob := jobStepFunc()
	r.step = step
	a := r.create(AgentCreate{Config: r.conf(100_000, 0, "w1")}, "web")
	ctx := context.Background()
	s, _, err := r.h.PostAgent(ctx, a.ID, "test", "", []Part{{Type: "text/plain", Body: []byte("go")}})
	if err != nil {
		t.Fatal(err)
	}
	<-sawJob // the model saw the job completion message
	r.settled(a.ID)
	job := findJobMessage(r, s)
	if job == "" {
		t.Fatalf("no job message in session %s", s)
	}
	assertJobMessage(t, job, tc.wantCut, tc.wantTail)
}

// TestJobTruncation covers the background-job completion message: a job whose
// log tail runs over the clip limits posts a message that announces the lines
// cut (the worker's tail-clamp drops plus the harness's final clip), and keeps
// only the trimmed tail. A job under the limits posts the whole tail with no
// notice. This is the path that, before the fix, silently discarded clip's cut
// count and showed a partial tail with no hint anything was missing.
func TestJobTruncation(t *testing.T) {
	for _, tc := range []jobCase{
		{
			name:     "clip-only",
			body:     bytes.Repeat([]byte("abcdefgh\n"), 3000), // over MaxLines, under 2*MaxBytes
			cut:      0,                                        // worker ships it whole
			wantCut:  "1000 lines cut",                         // clip cuts 1000
			wantTail: 2000,
		},
		{
			name:     "under-limits",
			body:     bytes.Repeat([]byte("abcdefgh\n"), 100),
			cut:      0,
			wantCut:  "", // no notice
			wantTail: 100,
		},
		{
			name:     "worker-and-clip",
			body:     bytes.Repeat([]byte("abcdefgh\n"), 2500), // over MaxLines
			cut:      1500,                                     // worker already dropped 1500
			wantCut:  "2000 lines cut",                         // 1500 + 500 from clip
			wantTail: 2000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runJobCase(t, tc)
		})
	}
}

// TestJobLostContact: when the worker connection drops after a job started,
// Remote.Job's done is called with the connection error, and the callback
// posts a system record (not a Message) keyed by the call id saying it may or
// may not still be running. This is the error path, which carries no tail and
// no cut notice; it must keep behaving this way after the fix.
func TestJobLostContact(t *testing.T) {
	r := newRig(t)
	r.mu.Lock()
	r.jobLoseContact = true
	r.mu.Unlock()
	sawErr := make(chan struct{}, 1)
	seen := 0
	r.step = func(body string) turn {
		seen++
		if seen == 1 {
			return turn{code: "ignored", bg: true, input: 10}
		}
		if strings.Contains(body, "may or may not still be running") {
			select {
			case sawErr <- struct{}{}:
			default:
			}
		}
		return turn{input: 10}
	}
	a := r.create(AgentCreate{Config: r.conf(100_000, 0, "w1")}, "web")
	ctx := context.Background()
	s, _, err := r.h.PostAgent(ctx, a.ID, "test", "", []Part{{Type: "text/plain", Body: []byte("go")}})
	if err != nil {
		t.Fatal(err)
	}
	<-sawErr // the model saw the lost-contact system record
	r.settled(a.ID)
	notes := r.bodies(s, statefile.System, "job")
	if len(notes) == 0 {
		t.Fatalf("no job system record in session %s", s)
	}
	last := notes[len(notes)-1]
	if !strings.Contains(last, "may or may not still be running") {
		t.Errorf("lost-contact record: %q", last)
	}
	if strings.Contains(last, "lines cut") {
		t.Errorf("lost-contact record should not announce a cut: %q", last)
	}
	// keyed by the call id so a reconnect does not double-post it
	chain := r.chain(s)
	var key string
	for _, rec := range chain {
		if rec.Kind == statefile.System && rec.Source == "job" {
			key = rec.IdempotencyKey
		}
	}
	if key == "" {
		t.Fatal("lost-contact record has no idempotency key")
	}
}

// TestFinishAnnounces is a regression guard for the run path's finish: a run
// result over the limits carries a Truncated flag and a first line naming the
// lines cut and the whole output path. The job path mirrors this in its
// posted message; this keeps the reference behavior it mirrors from drifting.
func TestFinishAnnounces(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh\n"), 3000)
	rec := statefile.Record{Header: statefile.Header{Type: "text/plain"}, Body: body}
	rec.FullOutput = "/scratch/output/big"
	exit := 0
	rec.Exit = &exit
	got := finish("run", args{Worker: "w1"}, rec, 0)
	if !got.Truncated {
		t.Error("run result over the limits is not marked Truncated")
	}
	first, _, _ := strings.Cut(string(got.Body), "\n")
	if first != "[1000 lines cut, whole output at /scratch/output/big]" {
		t.Errorf("run first line: %q", first)
	}
	// dropped from upstream (e.g. Remote.Run's clamp) folds into the note
	got = finish("run", args{Worker: "w1"}, rec, 250)
	first, _, _ = strings.Cut(string(got.Body), "\n")
	if first != "[1250 lines cut, whole output at /scratch/output/big]" {
		t.Errorf("run first line with upstream drops: %q", first)
	}
}
