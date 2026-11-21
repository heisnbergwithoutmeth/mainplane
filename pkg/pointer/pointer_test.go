package pointer

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// rt is a round tripper the tests inject into the package's HTTP client, so
// Find and Named are exercised against the real code path without the
// network.
type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// stubClient replaces the package client's transport with h for the test and
// restores it after.
func stubClient(t *testing.T, h http.RoundTripper) {
	t.Helper()
	old := client.Transport
	client.Transport = h
	t.Cleanup(func() { client.Transport = old })
}

func respond(req *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// keypair returns a fresh harness keypair and the key's base64 encoding, so
// decode(key) passes and Find reaches its pointer lookup.
func keypair(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv, Encode(priv)
}

// TestFindMissingRecord: a pointer 404 for a key with no record surfaces a
// friendly, actionable message -- not the bare "GET <host>/<key>: 404 Not
// Found" the user once saw. Mirrors Named, the sibling lookup that already
// maps errNotFound: the host, the key, and the raw 404 never reach the user.
func TestFindMissingRecord(t *testing.T) {
	stubClient(t, rt(func(req *http.Request) (*http.Response, error) {
		return respond(req, http.StatusNotFound, "")
	}))
	_, key := keypair(t)
	_, err := Find(context.Background(), key, "")
	if err == nil {
		t.Fatal("Find: nil error for a missing record")
	}
	if errors.Is(err, errNotFound) {
		t.Errorf("Find: a missing record's error wraps errNotFound, so the raw 404 leaks through: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"no harness is published", "gone", "30 days"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Find: error %q missing %q", msg, want)
		}
	}
	for _, leak := range []string{"404 Not Found", "pointer.mainplane.ai", key} {
		if strings.Contains(msg, leak) {
			t.Errorf("Find: error %q leaks %q (the raw 404, the host, or the key)", msg, leak)
		}
	}
}

// TestFindNon404Propagates: a pointer error that is not a 404 is still
// returned unwrapped -- the 404 mapping must not swallow other failures.
func TestFindNon404Propagates(t *testing.T) {
	stubClient(t, rt(func(req *http.Request) (*http.Response, error) {
		return respond(req, http.StatusInternalServerError, "boom")
	}))
	_, key := keypair(t)
	_, err := Find(context.Background(), key, "")
	if err == nil {
		t.Fatal("Find: nil error for a 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("Find: a 500 was not propagated: %v", err)
	}
	if strings.Contains(err.Error(), "no harness is published") {
		t.Fatalf("Find: a 500 was mapped as a missing record: %v", err)
	}
}

// TestFindHappyPath: a record the pointer signed, that the harness proves,
// returns its URL -- a full end-to-end Find, so the 404 mapping did not
// break the success path.
func TestFindHappyPath(t *testing.T) {
	priv, key := keypair(t)
	harnessURL := "https://harness.example.com"
	var seq int64 = 1
	stubClient(t, rt(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "pointer.mainplane.ai":
			rec := record{URL: harnessURL, Seq: seq, Sig: sign(priv, recordMsg(key, harnessURL, seq))}
			b, _ := json.Marshal(rec)
			return respond(req, http.StatusOK, string(b))
		default:
			nonce := req.URL.Query().Get("nonce")
			rec := record{URL: harnessURL, Sig: sign(priv, idMsg(harnessURL, nonce))}
			b, _ := json.Marshal(rec)
			return respond(req, http.StatusOK, string(b))
		}
	}))
	u, err := Find(context.Background(), key, "")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if u != harnessURL {
		t.Fatalf("Find: got %q, want %q", u, harnessURL)
	}
}
