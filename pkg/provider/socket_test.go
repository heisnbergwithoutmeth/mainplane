package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// socketProvider points a Socket: true openai-shaped provider at srv, the way
// OpenAI's own URL is on in production. The fake server's URL is http://; the
// websocket dialer takes it as ws:// internally.
func socketProvider(srv *httptest.Server) Provider {
	return Provider{
		URL:      srv.URL + "/responses",
		Key:      "k",
		Envelope: openai{ttl: 1800, anchor: "start", options: true},
		Headers:  bearer,
		Socket:   true,
	}
}

// basicReq is one user turn ahead of one system prompt: the smallest request
// that compiles to a Responses API body the socket stays happy with.
func basicReq(msg string) Request {
	return Request{
		Model: "gpt-5",
		Key:   "k",
		Context: []statefile.Record{
			{Header: statefile.Header{Kind: statefile.System, Type: "text/plain"}, Body: []byte("you are helpful")},
			{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain"}, Body: []byte(msg)},
		},
	}
}

// extendReq adds the prior step's text block and step record, then a fresh
// user message, so the next step chains on its previous_response_id (its input
// array starts with the prior step's input).
func extendReq(prev Request, text, stepID, msg string) Request {
	ctx := append([]statefile.Record(nil), prev.Context...)
	ctx = append(ctx,
		statefile.Record{Header: statefile.Header{Kind: statefile.Text, Type: "text/plain", Step: stepID}, Body: []byte(text)},
		statefile.Record{Header: statefile.Header{Kind: statefile.Step, ID: stepID}},
		statefile.Record{Header: statefile.Header{Kind: statefile.Message, Type: "text/plain"}, Body: []byte(msg)},
	)
	return Request{Model: prev.Model, Key: prev.Key, Context: ctx}
}

// frame writes one event to the WebSocket as a text frame.
func frame(ctx context.Context, c *websocket.Conn, raw string) error {
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, []byte(raw))
}

func createdEvent(id string) string {
	return fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id)
}

func itemDoneEvent(text string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":%q}]}}`, text)
}

func completedEvent(id string) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`, id)
}

// limitErrorEvent is the documented error event for the 60-minute cap.
// https://developers.openai.com/api/docs/guides/websocket-mode
func limitErrorEvent() string {
	return `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."}}`
}

// connCountingServer accepts a connection per message and runs handle with
// the connection's ordinal and the first message frame the client sent, so a
// test reads what was chained (or not) and how many sockets were opened.
func connCountingServer(t *testing.T, handle func(c *websocket.Conn, n int)) *httptest.Server {
	t.Helper()
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
		handle(c, n)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// readWSFrame reads one text message; a read failure ends the connection's loop.
func readWSFrame(ctx context.Context, c *websocket.Conn) ([]byte, bool) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, b, err := c.Read(rctx)
	if err != nil {
		return nil, false
	}
	return b, true
}

// previousResponseID extracts the previous_response_id field from a
// response.create the client sent, or "" if the field is missing or null.
func previousResponseID(frame []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(frame, &m) != nil {
		return ""
	}
	v, ok := m["previous_response_id"]
	if !ok || string(v) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// TestSocketStepHappyPath: a step over a Socket provider streams one item and
// completes, the emitted record carries the server's text, and the session's
// Conn is left chained for the next step.
func TestSocketStepHappyPath(t *testing.T) {
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		if n != 1 {
			t.Errorf("step 1 opened connection %d, want 1", n)
		}
		if _, ok := readWSFrame(context.Background(), c); !ok {
			return
		}
		_ = frame(context.Background(), c, createdEvent("resp1"))
		_ = frame(context.Background(), c, itemDoneEvent("hello"))
		_ = frame(context.Background(), c, completedEvent("resp1"))
	})
	p := socketProvider(srv)
	var conn Conn
	var emitted []statefile.Record
	hdr, err := p.Step(context.Background(), &conn, basicReq("hi"), func(r statefile.Record) { emitted = append(emitted, r) })
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if hdr.Stop != "completed" {
		t.Fatalf("step 1 stop: %q, want completed", hdr.Stop)
	}
	if len(emitted) != 1 || string(emitted[0].Body) != "hello" || emitted[0].Kind != statefile.Text {
		t.Fatalf("step 1 emitted: %+v", emitted)
	}
	if conn.id != "resp1" || conn.ws == nil {
		t.Fatalf("after step 1: conn.id=%q ws=%v", conn.id, conn.ws != nil)
	}
}

// TestSocketChainAcrossSteps: a second step on the same socket sends only the
// delta (with previous_response_id), and the server's second response
// replaces the first as the chain's id.
func TestSocketChainAcrossSteps(t *testing.T) {
	var gotFirst, gotSecond string
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		if n != 1 {
			t.Errorf("chained steps opened %d connections, want 1", n)
		}
		for i := 0; i < 2; i++ {
			in, ok := readWSFrame(context.Background(), c)
			if !ok {
				return
			}
			if i == 0 {
				gotFirst = previousResponseID(in)
			} else {
				gotSecond = previousResponseID(in)
			}
			id := fmt.Sprintf("resp%d", i+1)
			_ = frame(context.Background(), c, createdEvent(id))
			_ = frame(context.Background(), c, itemDoneEvent(fmt.Sprintf("answer-%d", i+1)))
			_ = frame(context.Background(), c, completedEvent(id))
		}
	})
	p := socketProvider(srv)
	var conn Conn
	req1 := basicReq("first")
	emit := func(emit *[]statefile.Record) func(statefile.Record) {
		return func(r statefile.Record) { *emit = append(*emit, r) }
	}
	first := []statefile.Record{}
	if _, err := p.Step(context.Background(), &conn, req1, emit(&first)); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	req2 := extendReq(req1, string(first[0].Body), "step1id", "second")
	second := []statefile.Record{}
	if _, err := p.Step(context.Background(), &conn, req2, emit(&second)); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if gotFirst != "" {
		t.Errorf("step 1 sent previous_response_id=%q, want none", gotFirst)
	}
	if gotSecond != "resp1" {
		t.Errorf("step 2 sent previous_response_id=%q, want resp1", gotSecond)
	}
	if conn.id != "resp2" {
		t.Errorf("after step 2: conn.id=%q, want resp2", conn.id)
	}
}

// TestSocketLimitCloseFrameMidStream is the bug's case: a step that begins on
// a socket under socketLife straddles the 60-minute cutoff, emits one item,
// then the server closes the connection. The error must be IsSocketLimit so
// the harness retries on a fresh socket; the failed step's Conn is wiped so
// the redial does not chain; the socket rotation never opened a second
// connection before the cutoff.
func TestSocketLimitCloseFrameMidStream(t *testing.T) {
	var mu sync.Mutex
	var conns int
	var step1PrevID, step2PrevID, step3PrevID string
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		mu.Lock()
		conns = n
		mu.Unlock()
		switch n {
		case 1:
			// Step 1 on connection 1: not chained.
			in1, ok := readWSFrame(context.Background(), c)
			if !ok {
				return
			}
			mu.Lock()
			step1PrevID = previousResponseID(in1)
			mu.Unlock()
			_ = frame(context.Background(), c, createdEvent("resp1"))
			_ = frame(context.Background(), c, itemDoneEvent("hello"))
			_ = frame(context.Background(), c, completedEvent("resp1"))
			// Step 2 on connection 1: chained on resp1, hits the limit mid-stream.
			in2, ok := readWSFrame(context.Background(), c)
			if !ok {
				return
			}
			mu.Lock()
			step2PrevID = previousResponseID(in2)
			mu.Unlock()
			_ = frame(context.Background(), c, createdEvent("resp2"))
			_ = frame(context.Background(), c, itemDoneEvent("partial"))
			_ = c.Close(websocket.StatusGoingAway, "60 minute socket limit")
		case 2:
			// Step 3 on the redial: NOT chained, since the failed step's Conn
			// was wiped. A whole input has no previous_response_id.
			in3, ok := readWSFrame(context.Background(), c)
			if !ok {
				return
			}
			mu.Lock()
			step3PrevID = previousResponseID(in3)
			mu.Unlock()
			_ = frame(context.Background(), c, createdEvent("resp3"))
			_ = frame(context.Background(), c, itemDoneEvent("recovered"))
			_ = frame(context.Background(), c, completedEvent("resp3"))
		}
	})
	p := socketProvider(srv)
	var conn Conn
	req1 := basicReq("first")
	var first []statefile.Record
	if _, err := p.Step(context.Background(), &conn, req1, func(r statefile.Record) { first = append(first, r) }); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	// A session that has aged wall-clock onto its socket reaches a straddling
	// step under socketLife, so the rotation check keeps the connection.
	conn.opened = time.Now().Add(-54 * time.Minute)

	req2 := extendReq(req1, string(first[0].Body), "step1", "second")
	var second []statefile.Record
	_, err := p.Step(context.Background(), &conn, req2, func(r statefile.Record) { second = append(second, r) })
	if err == nil {
		t.Fatal("step 2: err=nil, want the close-frame error wrapped as a socket limit")
	}
	if !IsSocketLimit(err) {
		t.Fatalf("step 2: err=%v IsSocketLimit=false, want true", err)
	}
	// The cut step's Conn is wiped so the next Step redials, and the rotation
	// check never closed the aged socket before the cutoff.
	if conn.ws != nil {
		t.Fatalf("step 2: conn.ws still open after the limit teardown")
	}
	if len(second) != 1 || string(second[0].Body) != "partial" {
		t.Fatalf("step 2 should have emitted one partial block before the close: %+v", second)
	}
	mu.Lock()
	c := conns
	mu.Unlock()
	if c != 1 {
		t.Fatalf("after step 2: conns=%d, want 1 (the rotation check did not redial)", c)
	}
	if step1PrevID != "" {
		t.Errorf("step 1 sent previous_response_id=%q, want none", step1PrevID)
	}
	if step2PrevID != "resp1" {
		t.Errorf("step 2 chained with previous_response_id=%q, want resp1", step2PrevID)
	}

	// Step 3 (the harness's redial): the Conn was wiped, so the request goes
	// whole over a fresh socket — no previous_response_id chain.
	var third []statefile.Record
	if _, err := p.Step(context.Background(), &conn, req2, func(r statefile.Record) { third = append(third, r) }); err != nil {
		t.Fatalf("step 3 (redial): %v", err)
	}
	if len(third) != 1 || string(third[0].Body) != "recovered" {
		t.Fatalf("step 3 emitted: %+v, want the redial's one item", third)
	}
	mu.Lock()
	c = conns
	mu.Unlock()
	if c != 2 {
		t.Fatalf("after step 3: conns=%d, want 2 (one redial)", c)
	}
	if step3PrevID != "" {
		t.Errorf("step 3 (redial) chained with previous_response_id=%q, want none", step3PrevID)
	}
	if conn.id != "resp3" {
		t.Errorf("after step 3: conn.id=%q, want resp3", conn.id)
	}
}

// TestSocketLimitErrorEventMidStream: the server's documented error event for
// the 60-minute cap (websocket_connection_limit_reached) is also IsSocketLimit.
// A chained step gets the envelope's event error; a fresh step would get an
// httpError that wraps the event's body too.
func TestSocketLimitErrorEventMidStream(t *testing.T) {
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		_, ok := readWSFrame(context.Background(), c)
		if !ok {
			return
		}
		_ = frame(context.Background(), c, createdEvent("resp2"))
		_ = frame(context.Background(), c, itemDoneEvent("partial"))
		_ = frame(context.Background(), c, limitErrorEvent())
		// Close after the error event, the way the docs imply the cutoff does.
		_ = c.Close(websocket.StatusGoingAway, "60 minute socket limit")
	})
	p := socketProvider(srv)
	// Set up a chained state by faking a prior completed response on Conn so
	// read takes the error event through the envelope's response.failed/error
	// path, which is what a chained step sees; otherwise a fresh step's read
	// hits the httpError branch first, which we test separately.
	var conn Conn
	conn.id = "resp0"
	conn.fields = mustMarshalBaseReq(t)
	conn.input = []json.RawMessage{[]byte(`{"role":"user","content":[{"type":"input_text","text":"first"}]}`)}
	conn.opened = time.Now().Add(-54 * time.Minute)
	req2 := extendReq(basicReq("first"), "earlier", "step1", "second")
	_, err := p.Step(context.Background(), &conn, req2, func(statefile.Record) {})
	if err == nil {
		t.Fatal("step 2: err=nil, want the limit error")
	}
	if !IsSocketLimit(err) {
		t.Fatalf("step 2: err=%v IsSocketLimit=false, want true", err)
	}
	if !strings.Contains(err.Error(), "websocket_connection_limit_reached") {
		t.Fatalf("step 2: err=%v does not surface the documented code", err)
	}
	if conn.ws != nil {
		t.Fatalf("step 2: conn.ws still open")
	}
}

// TestSocketLimitFreshStepErrorEvent: a fresh step (not chained) hits the
// limit error event in the read's httpError branch. IsSocketLimit must still
// be true, even though transient treats the httpError as non-retryable.
func TestSocketLimitFreshStepErrorEvent(t *testing.T) {
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		_, ok := readWSFrame(context.Background(), c)
		if !ok {
			return
		}
		_ = frame(context.Background(), c, limitErrorEvent())
		_ = c.Close(websocket.StatusGoingAway, "60 minute socket limit")
	})
	p := socketProvider(srv)
	var conn Conn
	_, err := p.Step(context.Background(), &conn, basicReq("hi"), func(statefile.Record) {})
	if err == nil {
		t.Fatal("step: err=nil, want the limit error")
	}
	if !IsSocketLimit(err) {
		t.Fatalf("err=%v IsSocketLimit=false, want true", err)
	}
}

// TestSocketNonLimitCloseNotWrapped: a close frame that is not the 60-minute
// cap stays on its own path, not IsSocketLimit. The harness must not redial
// on every transient close.
func TestSocketNonLimitCloseNotWrapped(t *testing.T) {
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		_, ok := readWSFrame(context.Background(), c)
		if !ok {
			return
		}
		_ = frame(context.Background(), c, createdEvent("resp2"))
		_ = frame(context.Background(), c, itemDoneEvent("partial"))
		// An abnormal close (1006's wire form is "no close frame", but the
		// server's StatusInternalError stands in for a non-going-away teardown
		// that is not the limit).
		_ = c.Close(websocket.StatusInternalError, "boom")
	})
	p := socketProvider(srv)
	var conn Conn
	conn.id = "resp0"
	conn.fields = mustMarshalBaseReq(t)
	conn.input = []json.RawMessage{[]byte(`{"role":"user","content":[{"type":"input_text","text":"first"}]}`)}
	conn.opened = time.Now().Add(-54 * time.Minute)
	req2 := extendReq(basicReq("first"), "earlier", "step1", "second")
	_, err := p.Step(context.Background(), &conn, req2, func(statefile.Record) {})
	if err == nil {
		t.Fatal("step: err=nil, want a close error")
	}
	if IsSocketLimit(err) {
		t.Fatalf("err=%v IsSocketLimit=true, want false (not the 60-minute cap)", err)
	}
}

// TestSocketLimitPreEmissionRetriesInProviderStep: a step that hits the limit
// before it has emitted anything is retried inside Provider.Step — each retry
// redials a fresh socket — and the loop returns the successful step header,
// not the limit error. This is the path that did not need the harness's help.
func TestSocketLimitPreEmissionRetriesInProviderStep(t *testing.T) {
	var mu sync.Mutex
	var conns int
	srv := connCountingServer(t, func(c *websocket.Conn, n int) {
		mu.Lock()
		conns = n
		mu.Unlock()
		if _, ok := readWSFrame(context.Background(), c); !ok {
			return
		}
		if n == 1 {
			// Connection 1: hit the cutoff before any output item is emitted.
			_ = frame(context.Background(), c, createdEvent("resp-doomed"))
			_ = c.Close(websocket.StatusGoingAway, "60 minute socket limit")
			return
		}
		// Connection 2 (the retry): the redial succeeds.
		_ = frame(context.Background(), c, createdEvent("resp-recover"))
		_ = frame(context.Background(), c, itemDoneEvent("recovered"))
		_ = frame(context.Background(), c, completedEvent("resp-recover"))
	})
	p := socketProvider(srv)
	var conn Conn
	var emitted []statefile.Record
	hdr, err := p.Step(context.Background(), &conn, basicReq("hi"), func(r statefile.Record) { emitted = append(emitted, r) })
	if err != nil {
		t.Fatalf("Step: %v (want a recovery after one pre-emission retry)", err)
	}
	if hdr.Stop != "completed" {
		t.Fatalf("Step stop: %q, want completed", hdr.Stop)
	}
	if len(emitted) != 1 || string(emitted[0].Body) != "recovered" {
		t.Fatalf("Step emitted: %+v, want the redial's one item", emitted)
	}
	mu.Lock()
	c := conns
	mu.Unlock()
	if c != 2 {
		t.Fatalf("Step opened %d connections, want 2 (one cutoff, one redial)", c)
	}
	if conn.id != "resp-recover" {
		t.Errorf("after Step: conn.id=%q, want resp-recover", conn.id)
	}
}

// TestSocketLimitDetection: the unexported detector matches both faces of
// the cutoff (a GoingAway close frame, with or without a reason; a close
// frame whose reason names the limit; and the error event's documented code
// in either the envelope's event error or the read's httpError body). It does
// not fire on other close codes or unrelated errors.
func TestSocketLimitDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"close going away with reason",
			fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "60 minute socket limit"}),
			true,
		},
		{
			"close going away without reason",
			fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusGoingAway}),
			true,
		},
		{
			"close reason naming the limit (case insensitive)",
			fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "Responses websocket CONNECTION LIMIT reached"}),
			true,
		},
		{
			"close reason 60 minute lowercased",
			fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusPolicyViolation, Reason: "the 60 Minute cap"}),
			true,
		},
		{
			"close internal error not limit",
			fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusInternalError, Reason: "boom"}),
			false,
		},
		{
			"envelope chained error event",
			fmt.Errorf("openai: %s", limitErrorEvent()),
			true,
		},
		{
			"httpError wrapping error event body",
			&httpError{name: "openai", status: "400 Bad Request", code: 400, body: []byte(limitErrorEvent())},
			true,
		},
		{
			"httpError unrelated",
			&httpError{name: "openai", status: "429 Too Many Requests", code: 429, body: []byte(`{"error":"rate"}`)},
			false,
		},
		{
			"bare network error",
			errors.New("dial tcp: connection refused"),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := socketLimit(tc.err); got != tc.want {
				t.Fatalf("socketLimit(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsSocketLimit: the exported predicate recognizes the wrapped type and
// keeps the underlying error unwrappable. Anything the harness sees that was
// not produced by socket stays false.
func TestIsSocketLimit(t *testing.T) {
	raw := fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "60 minute socket limit"})
	wrapped := &socketLimitError{err: raw}
	if !IsSocketLimit(wrapped) {
		t.Fatalf("IsSocketLimit(socketLimitError) = false, want true")
	}
	// The underlying close is still unwrappable, so transient and readers see
	// their closest reading.
	var ce websocket.CloseError
	if !errors.As(wrapped, &ce) || ce.Code != websocket.StatusGoingAway {
		t.Fatalf("errors.As(wrapped, &CloseError)=%v code=%v, want true / GoingAway", errors.As(wrapped, &ce), ce.Code)
	}
	if IsSocketLimit(raw) {
		t.Fatalf("IsSocketLimit(underlying) = true, want false (only the wrapped form is the harness's signal)")
	}
	if IsSocketLimit(nil) {
		t.Fatalf("IsSocketLimit(nil) = true, want false")
	}
	if IsSocketLimit(errors.New("unrelated")) {
		t.Fatalf("IsSocketLimit(unrelated) = true, want false")
	}
}

// mustMarshalBaseReq makes the non-input/non-stream fields a chained step's
// Conn.fields is compared against; the same as the request Compile produces
// for basicReq("first") after the socket's deletes.
func mustMarshalBaseReq(t *testing.T) []byte {
	t.Helper()
	p := socketProvider(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	body, _, err := p.Envelope.Compile(basicReq("first"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "input")
	delete(fields, "stream")
	b, err := marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
