package provider

import (
	"strings"
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// refusalItem is a Responses assistant message whose sole content part is a
// refusal: the model's reply rides in the refusal field, not text.
const refusalItem = `{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"Sorry, I can't help with that."}]}`

// TestRespRecordRefusalOnly: a refusal-only message must surface as a Text
// record carrying the refusal text. Before the fix, respPart had no Refusal
// field, the body came back empty, and respRecord returned (ok=false, err=nil),
// so the stream dropped the message and the step closed with no reply.
func TestRespRecordRefusalOnly(t *testing.T) {
	r, ok, err := respRecord([]byte(refusalItem))
	if err != nil || !ok {
		t.Fatalf("respRecord: ok=%t err=%v, want ok=true err=nil", ok, err)
	}
	if r.Kind != statefile.Text || r.Type != "text/plain" {
		t.Fatalf("record: kind=%s type=%s, want text/text/plain", r.Kind, r.Type)
	}
	if body := string(r.Body); body != "Sorry, I can't help with that." {
		t.Fatalf("body: %q, want the refusal text", body)
	}
}

// TestRespRecordMixedTextAndRefusal: a message with both an output_text part
// and a refusal part keeps both halves in order. Before the fix the refusal
// half was silently truncated to the text half.
func TestRespRecordMixedTextAndRefusal(t *testing.T) {
	item := []byte(`{"type":"message","role":"assistant","content":[` +
		`{"type":"output_text","text":"Here's how to do X."},` +
		`{"type":"refusal","refusal":"But I won't do X."}]}`)
	r, ok, err := respRecord(item)
	if err != nil || !ok {
		t.Fatalf("respRecord: ok=%t err=%v", ok, err)
	}
	if body := string(r.Body); body != "Here's how to do X.But I won't do X." {
		t.Fatalf("body: %q, want the text immediately followed by the refusal", body)
	}
}

// TestOpenAIStreamRefusal: end-to-end through openai{}.Stream with the SSE
// bytes a real Responses refusal produces. The refusal must reach emit as a
// Text record and Stream must return the step header with no error.
func TestOpenAIStreamRefusal(t *testing.T) {
	sse := "data: {\"type\":\"response.output_item.done\",\"item\":" + refusalItem + "}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	var got []statefile.Record
	h, err := (openai{}).Stream(strings.NewReader(sse), new(Cache), func(r statefile.Record) { got = append(got, r) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 1 || got[0].Kind != statefile.Text || string(got[0].Body) != "Sorry, I can't help with that." {
		t.Fatalf("emitted %d records: %+v, want one Text record with the refusal text", len(got), got)
	}
	if h.Stop != "completed" || h.Usage == nil || h.Usage.Output != 5 {
		t.Fatalf("header: %+v, want stop=completed output=5", h)
	}
}

// TestOpenAIRefusalRoundTrip: a refusal Text record is replayed by the envelope
// as a normal assistant output_text message, so a later turn returns the
// refusal body to the model with the rest of the context. Before the fix the
// refusal never reached the session file, so the next turn re-sent the same
// context without it and the model's answer was permanently lost.
func TestOpenAIRefusalRoundTrip(t *testing.T) {
	r, ok, err := respRecord([]byte(refusalItem))
	if err != nil || !ok {
		t.Fatalf("respRecord: ok=%t err=%v", ok, err)
	}
	items, err := (openai{}).assistant([]statefile.Record{r})
	if err != nil {
		t.Fatalf("assistant: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("assistant produced %d items, want 1: %+v", len(items), items)
	}
	msg, ok := items[0].(respMessage)
	if !ok {
		t.Fatalf("item type %T, want respMessage", items[0])
	}
	if msg.Role != "assistant" || len(msg.Content) != 1 ||
		msg.Content[0].Type != "output_text" || msg.Content[0].Text != "Sorry, I can't help with that." {
		t.Fatalf("message: %+v, want one output_text part carrying the refusal text", msg)
	}
}
