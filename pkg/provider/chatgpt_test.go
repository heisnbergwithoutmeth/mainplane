package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// tokenStub replaces http.DefaultClient's transport for this test so calls to
// OpenAI's token endpoint reach fn instead of the network; anything else is
// reported. The default transport is restored on cleanup.
func tokenStub(t *testing.T, fn func(*http.Request) *http.Response) {
	t.Helper()
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "auth.openai.com" || req.URL.Path != "/api/accounts/oauth/token" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
			return nil, errors.New("unexpected token request")
		}
		return fn(req), nil
	})
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
}

// tokenStubErr is tokenStub for a transport that returns an error, as an
// offline or timeout to the token endpoint does.
func tokenStubErr(t *testing.T, fn func(*http.Request) error) {
	t.Helper()
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "auth.openai.com" || req.URL.Path != "/api/accounts/oauth/token" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
			return nil, errors.New("unexpected token request")
		}
		return nil, fn(req)
	})
	t.Cleanup(func() { http.DefaultClient.Transport = prev })
}

// jsonResp builds an HTTP response with the given status and a JSON body.
func jsonResp(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

func writeCreds(t *testing.T, dir string, c chatgptCreds) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credentials), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCreds(t *testing.T, dir string) chatgptCreds {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, credentials))
	if err != nil {
		t.Fatal(err)
	}
	var c chatgptCreds
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// expiredCreds is a chatgpt.json whose access token is long past refreshEarly,
// so a Token call always enters the refresh branch. ClientID, Email and
// Subject should survive a refresh refusal so the next sign-in reuses the
// saved client and verifies the same account.
func expiredCreds() chatgptCreds {
	return chatgptCreds{
		Email:        "a@b.test",
		Subject:      "sub",
		ClientID:     "Client-keep-me",
		HostID:       "urn:uuid:deadbeef",
		IDToken:      "id-old",
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		Scope:        planScope,
		Expires:      time.Now().Add(-time.Hour),
	}
}

// TestTokenRefreshTerminalMapsReSignin: a refresh refusal with one of OpenAI's
// documented terminal OAuth codes is mapped to the same re-login configError
// the not-signed-in path uses, carries the code in the message, is terminal
// (the harness does not retry), clears the dead access and refresh tokens,
// and keeps the client id and identity fields for the next sign-in.
func TestTokenRefreshTerminalMapsReSignin(t *testing.T) {
	for _, code := range []string{
		"invalid_grant", "invalid_refresh_token", "token_expired",
		"refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused",
	} {
		t.Run(code, func(t *testing.T) {
			dir := t.TempDir()
			writeCreds(t, dir, expiredCreds())
			tokenStub(t, func(*http.Request) *http.Response {
				return jsonResp(http.StatusBadRequest, map[string]string{
					"error":             code,
					"error_description": "Refresh token expired",
				})
			})

			_, err := ChatGPT("", dir).Token(context.Background())
			if err == nil {
				t.Fatal("refresh refusal returned no error")
			}
			var ce configError
			if !errors.As(err, &ce) {
				t.Fatalf("terminal %s: error is %T, want configError: %v", code, err, err)
			}
			if !strings.Contains(err.Error(), "mainplane signin <provider>") {
				t.Fatalf("terminal %s: error lacks re-sign-in hint: %q", code, err.Error())
			}
			if !strings.Contains(err.Error(), code) {
				t.Fatalf("terminal %s: error lacks the OAuth code: %q", code, err.Error())
			}
			if transient(err) {
				t.Fatalf("terminal %s: marked transient; a re-login configError must be terminal", code)
			}
			after := readCreds(t, dir)
			if after.RefreshToken != "" {
				t.Fatalf("terminal %s: refresh token not cleared: %q", code, after.RefreshToken)
			}
			if after.AccessToken != "" {
				t.Fatalf("terminal %s: access token not cleared: %q", code, after.AccessToken)
			}
			if after.ClientID != "Client-keep-me" {
				t.Fatalf("terminal %s: client id lost: %q", code, after.ClientID)
			}
			if after.Email != "a@b.test" || after.Subject != "sub" {
				t.Fatalf("terminal %s: identity fields lost: %+v", code, after)
			}
		})
	}
}

// TestTokenAfterRefusalReadsNotSignedIn: after a terminal refusal has cleared
// the refresh token, a second Token call takes the not-signed-in branch and
// surfaces the same recovery instruction as a never-signed-in provider,
// without reaching the token endpoint.
func TestTokenAfterRefusalReadsNotSignedIn(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	calls := 0
	tokenStub(t, func(*http.Request) *http.Response {
		calls++
		return jsonResp(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	})

	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("first refusal did not error")
	}
	var ce configError
	if !errors.As(err, &ce) {
		t.Fatalf("first refusal: %T want configError", err)
	}
	if calls != 1 {
		t.Fatalf("first refusal: %d token calls, want 1", calls)
	}

	_, err = ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("second call did not error")
	}
	if !errors.As(err, new(configError)) {
		t.Fatalf("second call: %T want configError", err)
	}
	if !strings.Contains(err.Error(), "not signed in: mainplane signin <provider>") {
		t.Fatalf("second call: %q", err.Error())
	}
	if calls != 1 {
		t.Fatalf("second call reached the token endpoint %d time(s); the cleared refresh token must short-circuit", calls)
	}
}

// TestTokenRefresh5xxPassesThrough: a 5xx is a transient server error, not a
// terminal refresh refusal, so the error stays an *httpError and is retried by
// the harness; the credentials are untouched.
func TestTokenRefresh5xxPassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	tokenStub(t, func(*http.Request) *http.Response {
		return jsonResp(http.StatusBadGateway, map[string]string{"error": "server_error"})
	})

	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("5xx returned no error")
	}
	if errors.As(err, new(configError)) {
		t.Fatalf("5xx was mapped to configError: %v", err)
	}
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("5xx: error is %T want *httpError: %v", err, err)
	}
	if he.code != http.StatusBadGateway {
		t.Fatalf("5xx: code %d want %d", he.code, http.StatusBadGateway)
	}
	if !transient(err) {
		t.Fatalf("5xx: marked terminal; 5xx must be transient")
	}
	after := readCreds(t, dir)
	if after.RefreshToken != "refresh-old" || after.AccessToken != "access-old" {
		t.Fatalf("5xx: credentials were modified: %+v", after)
	}
}

// TestTokenRefresh429PassesThrough: a 429 is a transient rate limit, not a
// refresh refusal; it stays an *httpError and is retried, with the credentials
// kept.
func TestTokenRefresh429PassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	tokenStub(t, func(*http.Request) *http.Response {
		return jsonResp(http.StatusTooManyRequests, map[string]string{"error": "slow_down"})
	})

	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("429 returned no error")
	}
	if errors.As(err, new(configError)) {
		t.Fatalf("429 was mapped to configError: %v", err)
	}
	var he *httpError
	if !errors.As(err, &he) || he.code != http.StatusTooManyRequests {
		t.Fatalf("429: error is %v want *httpError code 429", err)
	}
	if !transient(err) {
		t.Fatalf("429: marked terminal; 429 must be transient")
	}
	after := readCreds(t, dir)
	if after.RefreshToken != "refresh-old" {
		t.Fatalf("429 cleared the refresh token: %+v", after)
	}
}

// TestTokenRefreshInvalidClientPassesThrough: invalid_client points at a
// client-configuration problem the user must fix in the provider config, not a
// re-login; it stays an *httpError, is terminal (a 400 is not retried) but is
// not a configError, and the credentials are kept.
func TestTokenRefreshInvalidClientPassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	tokenStub(t, func(*http.Request) *http.Response {
		return jsonResp(http.StatusBadRequest, map[string]string{"error": "invalid_client"})
	})

	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("invalid_client returned no error")
	}
	if errors.As(err, new(configError)) {
		t.Fatalf("invalid_client was mapped to a configError (it is a config problem, not a re-login): %v", err)
	}
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("invalid_client: error is %T want *httpError: %v", err, err)
	}
	if he.code != http.StatusBadRequest {
		t.Fatalf("invalid_client: code %d want 400", he.code)
	}
	if transient(err) {
		t.Fatalf("invalid_client marked transient; a 400 is terminal so no retry, but no token clearing either")
	}
	after := readCreds(t, dir)
	if after.RefreshToken != "refresh-old" {
		t.Fatalf("invalid_client cleared the refresh token: %+v", after)
	}
}

// TestTokenRefreshUnknownCodePassesThrough: an OAuth error code that is not on
// the terminal-refresh list is not a re-login signal; the raw httpError passes
// through so the user sees the actual server reply, and the credentials are
// kept.
func TestTokenRefreshUnknownCodePassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	tokenStub(t, func(*http.Request) *http.Response {
		return jsonResp(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	})

	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("unknown code returned no error")
	}
	if errors.As(err, new(configError)) {
		t.Fatalf("unknown code was mapped to a configError: %v", err)
	}
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("unknown code: error is %T want *httpError: %v", err, err)
	}
	after := readCreds(t, dir)
	if after.RefreshToken != "refresh-old" {
		t.Fatalf("unknown code cleared the refresh token: %+v", after)
	}
}

// TestTokenRefreshMalformedBodyPassesThrough: a 400 whose body has no
// {"error": ...} is not a known OAuth code; the raw httpError passes through,
// so the user sees the actual server reply, and the credentials are kept.
func TestTokenRefreshMalformedBodyPassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	tokenStub(t, func(*http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("400 Bad Request: gateway")),
		}
	})

	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("malformed 400 returned no error")
	}
	if errors.As(err, new(configError)) {
		t.Fatalf("malformed 400 was mapped to configError: %v", err)
	}
	var he *httpError
	if !errors.As(err, &he) {
		t.Fatalf("malformed 400: error is %T want *httpError: %v", err, err)
	}
	if !bytes.Contains(he.body, []byte("gateway")) {
		t.Fatalf("malformed 400: body lost: %q", he.body)
	}
	after := readCreds(t, dir)
	if after.RefreshToken != "refresh-old" {
		t.Fatalf("malformed 400 cleared the refresh token: %+v", after)
	}
}

// TestTokenRefreshNetworkErrorPassesThrough: a transport error from the token
// endpoint (offline, timeout) is returned verbatim; the credentials are
// kept, since the failure says nothing about them, and transient() reports it
// retried.
func TestTokenRefreshNetworkErrorPassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	netErr := errors.New("dial tcp: unreachable")
	tokenStubErr(t, func(*http.Request) error { return netErr })

	_, err := ChatGPT("", dir).Token(context.Background())
	if !errors.Is(err, netErr) {
		t.Fatalf("network error: got %v want %v", err, netErr)
	}
	if !transient(err) {
		t.Fatalf("network error marked terminal; a transport failure must retry")
	}
	after := readCreds(t, dir)
	if after.RefreshToken != "refresh-old" {
		t.Fatalf("network error cleared the refresh token: %+v", after)
	}
}

// TestTokenRefreshSuccessRotatesCreds: a successful refresh rotates the access
// and refresh tokens (OpenAI's refresh rotates both), advances the expiry,
// keeps the client id, and persists the new credentials to chatgpt.json.
func TestTokenRefreshSuccessRotatesCreds(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, expiredCreds())
	calls := 0
	tokenStub(t, func(*http.Request) *http.Response {
		calls++
		return jsonResp(http.StatusOK, map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
			"id_token":      "id-new",
			"scope":         planScope,
			"expires_in":    3600,
		})
	})

	tok, err := ChatGPT("", dir).Token(context.Background())
	if err != nil {
		t.Fatalf("refresh success: %v", err)
	}
	if tok != "access-new" {
		t.Fatalf("refresh success: token %q want access-new", tok)
	}
	if calls != 1 {
		t.Fatalf("refresh success: %d token calls, want 1", calls)
	}
	after := readCreds(t, dir)
	if after.AccessToken != "access-new" || after.RefreshToken != "refresh-new" {
		t.Fatalf("refresh success: creds not rotated: %+v", after)
	}
	if !time.Now().Before(after.Expires) {
		t.Fatalf("refresh success: expiry not advanced: %v", after.Expires)
	}
	if after.ClientID != "Client-keep-me" {
		t.Fatalf("refresh success: client id changed: %q", after.ClientID)
	}
}

// TestTokenFreshShortCircuits: an access token still within refreshEarly of
// expiry is returned with no call to the token endpoint.
func TestTokenFreshShortCircuits(t *testing.T) {
	dir := t.TempDir()
	c := expiredCreds()
	c.AccessToken = "access-fresh"
	c.RefreshToken = "refresh-fresh"
	c.Expires = time.Now().Add(refreshEarly + 30*time.Second)
	writeCreds(t, dir, c)
	tokenStub(t, func(*http.Request) *http.Response {
		t.Fatal("token endpoint called for a still-fresh access token")
		return nil
	})

	tok, err := ChatGPT("", dir).Token(context.Background())
	if err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	if tok != "access-fresh" {
		t.Fatalf("fresh token: got %q want access-fresh", tok)
	}
}

// TestTokenNeverSignedIn: with no chatgpt.json at all, Token surfaces the
// not-signed-in configError the recovery instructions come from, without any
// network call.
func TestTokenNeverSignedIn(t *testing.T) {
	dir := t.TempDir()
	tokenStub(t, func(*http.Request) *http.Response {
		t.Fatal("token endpoint called when no creds file exists")
		return nil
	})
	_, err := ChatGPT("", dir).Token(context.Background())
	if err == nil {
		t.Fatal("never signed in returned no error")
	}
	var ce configError
	if !errors.As(err, &ce) {
		t.Fatalf("never signed in: %T want configError: %v", err, err)
	}
	if !strings.Contains(err.Error(), "not signed in: mainplane signin <provider>") {
		t.Fatalf("never signed in: %q", err.Error())
	}
}
