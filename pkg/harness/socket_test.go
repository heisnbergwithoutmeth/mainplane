package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mainplane-ai/mainplane/pkg/provider"
	"github.com/mainplane-ai/mainplane/pkg/statefile"
	"github.com/mainplane-ai/mainplane/pkg/worker"
)

// socketHarness builds a harness whose "fake" provider speaks the OpenAI
// Responses WebSocket at srv. Reuses the rig's worker-less plumbing: no apps,
// no agents, just a session, so a Socket: true Step goes through the
// provider's dial-chain-redial path end-to-end.
func socketHarness(t *testing.T, srv *httptest.Server) *Harness {
	t.Helper()
	dir := t.TempDir()
	p := provider.OpenAI("", "k")
	p.URL = srv.URL + "/responses"
	h := &Harness{
		Sessions:  statefile.Sessions{Dir: filepath.Join(dir, "sessions")},
		Providers: map[string]provider.Provider{"fake": p},
		Workers:   NewPool(func(string) worker.Desired { return worker.Desired{} }, func(string) string { return "" }),
		Prompt:    func() ([]string, error) { return nil, nil },
		Agents:    filepath.Join(dir, "agents"),
		Skill:     func(string) (string, bool) { return "", false },
	}
	for _, d := range []string{h.Sessions.Dir, h.Agents} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.LoadAgents(); err != nil {
		t.Fatal(err)
	}
	return h
}

// wsFrame writes one event to a server-side websocket as a text frame.
func wsFrame(ctx context.Context, c *websocket.Conn, raw string) error {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, []byte(raw))
}

func wsCreated(id string) string {
	return fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id)
}

func wsItemDone(text string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":%q}]}}`, text)
}

func wsCompleted(id string) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`, id)
}

// wsReadFrame reads one frame on a server-side websocket; a read failure ends
// the connection's loop.
func wsReadFrame(ctx context.Context, c *websocket.Conn) ([]byte, bool) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, b, err := c.Read(rctx)
	if err != nil {
		return nil, false
	}
	return b, true
}

// TestSocketHarnessRetriesLimitMidStream: a session whose step straddles
// OpenAI's 60-minute WebSocket cap ends the step closed, not failed, after
// the harness reconnects on a fresh socket with a fresh step id. The cut
// step's emitted block stays in the file (append-only) but is dropped from
// the model's context, since no step record closes it.
func TestSocketHarnessRetriesLimitMidStream(t *testing.T) {
	var mu sync.Mutex
	var conns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()
		ctx := context.Background()
		switch n {
		case 1:
			// step 1: not chained; respond with one text and close the turn.
			if _, ok := wsReadFrame(ctx, c); !ok {
				return
			}
			_ = wsFrame(ctx, c, wsCreated("resp1"))
			_ = wsFrame(ctx, c, wsItemDone("hello"))
			_ = wsFrame(ctx, c, wsCompleted("resp1"))
			// step 2 (chained): the cutoff tears down mid-stream.
			if _, ok := wsReadFrame(ctx, c); !ok {
				return
			}
			_ = wsFrame(ctx, c, wsCreated("resp2"))
			_ = wsFrame(ctx, c, wsItemDone("partial"))
			_ = c.Close(websocket.StatusGoingAway, "60 minute socket limit")
		case 2:
			// step 2 retry (redial): the harness reconnects with a fresh step
			// id, so the request is whole (no previous_response_id).
			if _, ok := wsReadFrame(ctx, c); !ok {
				return
			}
			_ = wsFrame(ctx, c, wsCreated("resp3"))
			_ = wsFrame(ctx, c, wsItemDone("recovered"))
			_ = wsFrame(ctx, c, wsCompleted("resp3"))
		}
	}))
	t.Cleanup(srv.Close)

	h := socketHarness(t, srv)
	id, err := h.Create(context.Background(), Create{Model: "fake/gpt-5", ContextLimit: 10000}, "test")
	if err != nil {
		t.Fatal(err)
	}
	// h.note writes a record without kicking the runner, so the test owns
	// when h.Step runs and the socket is reused from step to step (the way a
	// long Run inside a real session keeps Conn across steps until the run
	// ends). h.Post would race the runner against the test's own h.Step here.
	note := func(text string) {
		if err := h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte(text)}); err != nil {
			t.Fatal(err)
		}
	}
	note("hi")

	// step 1: no calls → run is closed after it.
	if status, err := h.Step(context.Background(), id); err != nil || status != statefile.StatusClosed {
		t.Fatalf("step 1: status=%s err=%v", status, err)
	}

	// A second message reopens the session for the straddling step (the run
	// between steps is what consumed the socket's age in a real session,
	// here the same socket still has c.id="resp1", so the next step chains).
	note("again")

	// step 2: the harness retries past the cut, so it ends closed, not failed.
	if status, err := h.Step(context.Background(), id); err != nil {
		t.Fatalf("step 2: err=%v (the harness should have retried past the limit)", err)
	} else if status != statefile.StatusClosed {
		t.Fatalf("step 2: status=%s, want closed", status)
	}

	mu.Lock()
	c := conns
	mu.Unlock()
	if c != 2 {
		t.Errorf("server saw %d connections, want 2 (one before the cutoff, one redial)", c)
	}

	chain, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := statefile.Derive(chain); got != statefile.StatusClosed {
		t.Errorf("session status: %s, want closed (no manual /retry needed)", got)
	}

	// The cut step's emitted block is in the file but no step record closes
	// it, so statefile.Build drops it from the model's context.
	var orphanInChain, recoveredInChain bool
	for _, r := range chain {
		if r.Kind == statefile.Text {
			switch string(r.Body) {
			case "partial":
				orphanInChain = true
			case "recovered":
				recoveredInChain = true
			}
		}
	}
	if !orphanInChain {
		t.Errorf("the failed step's partial block is not in the session file")
	}
	if !recoveredInChain {
		t.Errorf("the retry's recovered block is not in the session file")
	}
	built, _ := statefile.Build(chain)
	var reached []string
	for _, r := range built {
		if r.Kind == statefile.Text {
			reached = append(reached, string(r.Body))
		}
	}
	// hello: closed by step 1, in context. recovered: closed by the retry's
	// step record, in context. partial: the cut step's emitted block, no step
	// record closes it, dropped by Build.
	want := []string{"hello", "recovered"}
	if len(reached) != len(want) || reached[0] != want[0] || reached[1] != want[1] {
		t.Errorf("model context text blocks: %v, want %v (partial dropped)", reached, want)
	}
}

// TestSocketHarnessRetriesFailedPastLimit: if the cutoff strikes every retry
// too, the harness falls through after socketLimitRetries to a failed
// session, with the limit error visible to the user — no infinite reconnect.
func TestSocketHarnessRetriesFailedPastLimit(t *testing.T) {
	var mu sync.Mutex
	var conns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		conns++
		mu.Unlock()
		ctx := context.Background()
		if _, ok := wsReadFrame(ctx, c); !ok {
			return
		}
		_ = wsFrame(ctx, c, wsCreated("resp"))
		_ = wsFrame(ctx, c, wsItemDone("partial"))
		_ = c.Close(websocket.StatusGoingAway, "60 minute socket limit")
	}))
	t.Cleanup(srv.Close)

	h := socketHarness(t, srv)
	id, err := h.Create(context.Background(), Create{Model: "fake/gpt-5", ContextLimit: 10000}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.note(h.session(id), id, statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain", Via: "test"}, Body: []byte("hi")}); err != nil {
		t.Fatal(err)
	}

	status, stepErr := h.Step(context.Background(), id)
	if stepErr != nil {
		t.Fatalf("step: unexpected error: %v", stepErr)
	}
	if status != statefile.StatusFailed {
		t.Fatalf("step: status=%s, want failed (the limit could not be recovered past %d retries)", status, socketLimitRetries)
	}
	mu.Lock()
	c := conns
	mu.Unlock()
	// 1 attempt + socketLimitRetries reconnects.
	if want := 1 + socketLimitRetries; c != want {
		t.Errorf("server saw %d connections, want %d (one attempt and %d reconnects)", c, want, socketLimitRetries)
	}
	chain, err := h.Sessions.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := statefile.Derive(chain); got != statefile.StatusFailed {
		t.Errorf("session status: %s, want failed (the limit could not be recovered)", got)
	}
	// The user-facing error names the documented limit so the manual /retry
	// path can fall back on it.
	var sawLimitErr bool
	for _, r := range chain {
		if r.Kind == statefile.Error && (strings.Contains(string(r.Body), "60 minute socket limit") || strings.Contains(string(r.Body), "websocket_connection_limit_reached")) {
			sawLimitErr = true
		}
	}
	if !sawLimitErr {
		t.Errorf("the failed session's error record does not name the limit: %v", chain)
	}
}
