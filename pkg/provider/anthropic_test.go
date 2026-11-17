package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

var testTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const thinkingBody = `{"type":"thinking","thinking":"reasoning","signature":"sig"}`

// thinkingOnlyChain builds a record chain whose context (after statefile.Build)
// ends with one closed assistant turn holding a single Thinking record tagged
// with the given provider. A user message precedes the turn.
func thinkingOnlyChain(provider string) []statefile.Record {
	return []statefile.Record{
		{Header: statefile.Header{N: 1, Kind: statefile.Start, ID: "a1a1a1a1", Time: testTime}},
		{Header: statefile.Header{N: 2, Kind: statefile.Config, ID: "b2b2b2b2", Time: testTime}, Body: []byte(`{}`)},
		{Header: statefile.Header{N: 3, Kind: statefile.Message, ID: "c3c3c3c3", Type: "text/plain", Time: testTime, Via: "user"}, Body: []byte("hello")},
		{Header: statefile.Header{N: 4, Kind: statefile.Thinking, ID: "d4d4d4d4", Step: "e5e5e5e5", Provider: provider, Type: "application/json", Time: testTime}, Body: []byte(thinkingBody)},
		{Header: statefile.Header{N: 5, Kind: statefile.Step, ID: "e5e5e5e5", Upto: 3, Model: "claude-sonnet-4-20250514", Time: testTime}},
	}
}

// textAndThinkingChain is like thinkingOnlyChain but the assistant turn also
// holds a Text record, so the turn is non-empty even when the thinking is
// filtered out.
func textAndThinkingChain(provider string) []statefile.Record {
	return []statefile.Record{
		{Header: statefile.Header{N: 1, Kind: statefile.Start, ID: "a1a1a1a1", Time: testTime}},
		{Header: statefile.Header{N: 2, Kind: statefile.Config, ID: "b2b2b2b2", Time: testTime}, Body: []byte(`{}`)},
		{Header: statefile.Header{N: 3, Kind: statefile.Message, ID: "c3c3c3c3", Type: "text/plain", Time: testTime, Via: "user"}, Body: []byte("hello")},
		{Header: statefile.Header{N: 4, Kind: statefile.Text, ID: "d4d4d4d4", Step: "e5e5e5e5", Type: "text/plain", Time: testTime}, Body: []byte("response")},
		{Header: statefile.Header{N: 5, Kind: statefile.Thinking, ID: "f6f6f6f6", Step: "e5e5e5e5", Provider: provider, Type: "application/json", Time: testTime}, Body: []byte(thinkingBody)},
		{Header: statefile.Header{N: 6, Kind: statefile.Step, ID: "e5e5e5e5", Upto: 3, Model: "claude-sonnet-4-20250514", Time: testTime}},
	}
}

// noNullContent fails the test if body contains "content":null, which the
// Messages API rejects with a 400.
func noNullContent(t *testing.T, body []byte) {
	t.Helper()
	if strings.Contains(string(body), `"content":null`) {
		t.Errorf("body contains \"content\":null: %s", body)
	}
}

// TestAnthropicCompileSkipsForeignThinkingOnlyTurn tests that the anthropic
// envelope skips an assistant turn whose only record is a Thinking record
// signed by another provider (or untagged), instead of emitting a message with
// "content": null.
func TestAnthropicCompileSkipsForeignThinkingOnlyTurn(t *testing.T) {
	for _, provider := range []string{"openai", "", "claude-code", "bedrock", "google"} {
		t.Run(provider, func(t *testing.T) {
			ctx, _ := statefile.Build(thinkingOnlyChain(provider))
			body, _, err := anthropic{}.Compile(Request{Model: "claude-sonnet-4-20250514", Context: ctx})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			noNullContent(t, body)
			var req struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal: %v: %s", err, body)
			}
			var hasUser bool
			for _, m := range req.Messages {
				if m.Role == "assistant" {
					t.Errorf("assistant turn should be skipped for thinking provider %q: %s", provider, body)
				}
				if m.Role == "user" {
					hasUser = true
				}
			}
			if !hasUser {
				t.Errorf("user turn missing: %s", body)
			}
		})
	}
}

// TestAnthropicCompileReplaysNativeThinking tests that a native-signed thinking
// block is replayed as an assistant message with the thinking content.
func TestAnthropicCompileReplaysNativeThinking(t *testing.T) {
	ctx, _ := statefile.Build(thinkingOnlyChain("anthropic"))
	body, _, err := anthropic{}.Compile(Request{Model: "claude-sonnet-4-20250514", Context: ctx})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	noNullContent(t, body)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	var hasAssistant bool
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			hasAssistant = true
			if len(m.Content) == 0 || string(m.Content) == "null" {
				t.Errorf("assistant content is empty or null: %s", body)
			}
		}
	}
	if !hasAssistant {
		t.Errorf("assistant turn missing for native thinking: %s", body)
	}
}

// TestAnthropicCompileTextWithForeignThinking tests that a turn with both text
// and foreign thinking includes the text (which is not provider-filtered) and
// skips only the foreign thinking.
func TestAnthropicCompileTextWithForeignThinking(t *testing.T) {
	ctx, _ := statefile.Build(textAndThinkingChain("openai"))
	body, _, err := anthropic{}.Compile(Request{Model: "claude-sonnet-4-20250514", Context: ctx})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	noNullContent(t, body)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	var hasAssistant bool
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			hasAssistant = true
			if !strings.Contains(string(m.Content), "response") {
				t.Errorf("assistant content missing text: %s", m.Content)
			}
		}
	}
	if !hasAssistant {
		t.Errorf("assistant turn missing for text+foreign thinking: %s", body)
	}
}

// TestClaudeCodeCompileSkipsForeignThinkingOnlyTurn tests that the claude-code
// envelope skips an assistant frame whose content is empty (foreign thinking
// only), instead of emitting a frame with "content": null.
func TestClaudeCodeCompileSkipsForeignThinkingOnlyTurn(t *testing.T) {
	for _, provider := range []string{"openai", "", "anthropic", "bedrock", "google"} {
		t.Run(provider, func(t *testing.T) {
			ctx, _ := statefile.Build(thinkingOnlyChain(provider))
			body, _, err := claudeCode{}.Compile(Request{Model: "claude-sonnet-4-20250514", Context: ctx})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			noNullContent(t, body)
			var req struct {
				Frames []struct {
					Type    string          `json:"type"`
					Message json.RawMessage `json:"message"`
				} `json:"frames"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal: %v: %s", err, body)
			}
			for _, f := range req.Frames {
				if f.Type == "assistant" {
					t.Errorf("assistant frame should be skipped for thinking provider %q: %s", provider, body)
				}
			}
		})
	}
}

// TestClaudeCodeCompileReplaysNativeThinking tests that a native-signed thinking
// block is replayed as an assistant frame with the thinking content.
func TestClaudeCodeCompileReplaysNativeThinking(t *testing.T) {
	ctx, _ := statefile.Build(thinkingOnlyChain("claude-code"))
	body, _, err := claudeCode{}.Compile(Request{Model: "claude-sonnet-4-20250514", Context: ctx})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	noNullContent(t, body)
	var req struct {
		Frames []struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	var hasAssistant bool
	for _, f := range req.Frames {
		if f.Type == "assistant" {
			hasAssistant = true
			var msg struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(f.Message, &msg); err != nil {
				t.Fatalf("unmarshal message: %v: %s", err, f.Message)
			}
			if len(msg.Content) == 0 || string(msg.Content) == "null" {
				t.Errorf("assistant content is empty or null: %s", body)
			}
		}
	}
	if !hasAssistant {
		t.Errorf("assistant frame missing for native thinking: %s", body)
	}
}

// TestClaudeCodeCompileTextWithForeignThinking tests that a turn with both text
// and foreign thinking includes the text in an assistant frame.
func TestClaudeCodeCompileTextWithForeignThinking(t *testing.T) {
	ctx, _ := statefile.Build(textAndThinkingChain("openai"))
	body, _, err := claudeCode{}.Compile(Request{Model: "claude-sonnet-4-20250514", Context: ctx})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	noNullContent(t, body)
	var req struct {
		Frames []struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	var hasAssistant bool
	for _, f := range req.Frames {
		if f.Type == "assistant" {
			hasAssistant = true
			var msg struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(f.Message, &msg); err != nil {
				t.Fatalf("unmarshal message: %v: %s", err, f.Message)
			}
			if !strings.Contains(string(msg.Content), "response") {
				t.Errorf("assistant content missing text: %s", msg.Content)
			}
		}
	}
	if !hasAssistant {
		t.Errorf("assistant frame missing for text+foreign thinking: %s", body)
	}
}
