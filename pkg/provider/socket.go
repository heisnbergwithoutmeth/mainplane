package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// OpenAI's Responses API takes a step over a WebSocket too:
// https://developers.openai.com/api/docs/guides/websocket-mode. The server
// keeps the last response of the socket in memory, so a step that continues it
// sends only the input past it, chained by previous_response_id, even with
// store false. The step record's request hash stays the full body's: the
// delta is transport only.
//
// The server ends a socket at 60 minutes; socketLife opens a new one before a
// step when it can. A step that straddles the cutoff the check cannot bound
// hits the limit mid-stream: socketLimitError marks that teardown, so the
// harness recovers on a fresh socket instead of failing the session.
const socketLife = 55 * time.Minute

// socketLimitError marks the 60-minute Responses WebSocket cap. OpenAI's docs
// name two faces of the same cutoff, and either reaches here: a close frame
// (StatusGoingAway, reason naming the limit) or an error event (type "error",
// code "websocket_connection_limit_reached", status 400). It unwraps to the
// underlying error so transient and readers see their closest reading. A
// retry's fresh socket is its recovery, but the cut step has already emitted,
// so Step's terminal-after-emission gate is left to stand and the harness
// retries on a new step id whose built context drops the orphaned blocks.
type socketLimitError struct{ err error }

func (e *socketLimitError) Error() string { return e.err.Error() }
func (e *socketLimitError) Unwrap() error { return e.err }

// IsSocketLimit reports whether err is the 60-minute Responses WebSocket cap.
// The harness uses it as a reconnection point: a fresh step id and a redial
// resend the step's input whole. Anything else, a network blip included,
// keeps the path it always had.
func IsSocketLimit(err error) bool {
	var sle *socketLimitError
	return errors.As(err, &sle)
}

// socketLimit reports whether err carries the documented signature of the
// 60-minute teardown. The envelope surfaces the close frame in a wrapped
// websocket.CloseError and the error event's raw body in its message, so both
// faces reach this check with the strings and code the server emits.
func socketLimit(err error) bool {
	if err == nil {
		return false
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		// The cutoff's close is the endpoint-going-away code (a 60-minute cap is
		// a planned teardown, and a proxy may strip the reason). A reason that
		// names the limit matches on its own, in case a server uses a custom
		// code. Other close codes stay on their own path: a network blip is
		// transient under unrelated errors.
		if ce.Code == websocket.StatusGoingAway ||
			strings.Contains(strings.ToLower(ce.Reason), "60 minute") ||
			strings.Contains(strings.ToLower(ce.Reason), "connection limit") {
			return true
		}
	}
	// The error event's body holds the documented code as a literal; the
	// envelope surfaces raw bytes, whether the read path took it as an httpError
	// (a fresh step, unchained) or as the envelope's own error (a chained one).
	return strings.Contains(err.Error(), "websocket_connection_limit_reached")
}

// Conn is a session's socket, kept from step to step while the session steps.
// The zero value has none; a failed step closes it, and the next step opens a
// new one and sends its input whole. A session steps one at a time, so Conn
// has no lock.
type Conn struct {
	ws     *websocket.Conn
	url    string
	header http.Header // the handshake's: auth that changes, such as a refreshed token or another provider, needs a new socket
	opened time.Time
	fields []byte            // the last request without its input and stream
	input  []json.RawMessage // what the server holds as the last response: its input, then its output compiled as input
	id     string            // the last response's id
}

// Close ends the socket.
func (c *Conn) Close() {
	if c.ws != nil {
		_ = c.ws.CloseNow()
	}
	*c = Conn{}
}

// socket is one attempt over the session's socket, the WebSocket twin of once.
// A continuation that fails is not httpError, so it is retried, whole, on a
// new socket: the server may have dropped the response it chained from. A
// failure that hits the 60-minute cap is wrapped as socketLimitError, so the
// harness retries past the terminal-after-emission gate on a fresh socket.
func (p Provider) socket(ctx context.Context, c *Conn, url, session string, body []byte, emit func(statefile.Record)) (h statefile.Header, emitted bool, err error) {
	defer func() {
		if err != nil {
			c.Close()
			if socketLimit(err) {
				err = &socketLimitError{err: err}
			}
		}
	}()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return h, false, err
	}
	var input []json.RawMessage
	if err := json.Unmarshal(fields["input"], &input); err != nil {
		return h, false, err
	}
	delete(fields, "input")
	delete(fields, "stream") // the socket streams; the docs leave transport fields out
	same, err := marshal(fields)
	if err != nil {
		return h, false, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return h, false, err
	}
	if err := p.header(ctx, hr, session); err != nil {
		return h, false, err
	}
	if c.ws != nil && (c.url != url || !maps.EqualFunc(c.header, hr.Header, slices.Equal) || time.Since(c.opened) > socketLife) {
		c.Close()
	}
	if c.ws == nil {
		if err := c.dial(ctx, p.Envelope.Name(), url, hr.Header); err != nil {
			return h, false, err
		}
	}
	send, chained := input, c.id != "" && bytes.Equal(same, c.fields) && len(input) >= len(c.input) &&
		slices.EqualFunc(input[:len(c.input)], c.input, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) })
	if chained {
		send = input[len(c.input):]
		if fields["previous_response_id"], err = marshal(c.id); err != nil {
			return h, false, err
		}
	}
	fields["type"] = json.RawMessage(`"response.create"`)
	if fields["input"], err = marshal(send); err != nil {
		return h, false, err
	}
	frame, err := marshal(fields)
	if err != nil {
		return h, false, err
	}
	wctx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	if err := c.ws.Write(wctx, websocket.MessageText, frame); err != nil {
		return h, false, err
	}
	name := p.Envelope.Name()
	var out []statefile.Record
	tagged := func(r statefile.Record) {
		emitted = true
		if r.Kind == statefile.Thinking {
			r.Provider = name
		}
		out = append(out, r)
		emit(r)
	}
	var s respStream
	for !s.done {
		if err := c.read(ctx, name, chained, &s, tagged); err != nil {
			return s.header(), emitted, err
		}
	}
	items, err := openai{}.assistant(out)
	if err != nil {
		return h, emitted, err
	}
	for _, it := range items {
		b, err := marshal(it)
		if err != nil {
			return h, emitted, err
		}
		input = append(input, b)
	}
	c.fields, c.input, c.id = same, input, s.id
	return s.header(), emitted, nil
}

// dial opens the socket with the headers a request over HTTP has. A refused
// upgrade is httpError, so a 401 is not retried and a 429 or 5xx is.
func (c *Conn) dial(ctx context.Context, name, url string, header http.Header) error {
	ctx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			msg, _ := io.ReadAll(resp.Body)
			return &httpError{name: name, status: resp.Status, code: resp.StatusCode, body: msg, after: retryAfter(resp.Header)}
		}
		return err
	}
	ws.SetReadLimit(maxEvent)
	c.ws, c.url, c.header, c.opened = ws, url, header, time.Now()
	return nil
}

// read takes one event. idle bounds the wait for it. An error event with a
// status is httpError, as the same refusal over HTTP would be, unless it
// answers a continuation.
func (c *Conn) read(ctx context.Context, name string, chained bool, s *respStream, emit func(statefile.Record)) error {
	ctx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: no event for %s", name, idle)
	}
	if err != nil {
		return err
	}
	var e struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}
	if e.Type == "error" && e.Status != 0 && !chained {
		return &httpError{name: name, status: fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status)), code: e.Status, body: data}
	}
	return s.event(data, emit)
}
