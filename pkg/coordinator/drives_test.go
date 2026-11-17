package coordinator

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
)

// win is a Windows worker node named name, the only kind that gets an SMB
// user. A Linux or macOS client, or one whose Hostinfo is missing, is what
// smb returns nil for, and a Windows client is the one the bug bit.
func win(name string) *node {
	return &node{Name: name, Hostinfo: &tailcfg.Hostinfo{OS: "windows"}}
}

// hmacHex is the harness's SMB-password HMAC of msg with key, the value the
// coordinator hands to the worker without storing. Computes it independently
// of smb so a regression in its derivation shows up as a mismatch.
func hmacHex(key []byte, msg string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

// userAddOK is whether useradd on the distros a drive server may run (shadow
// ≤ 4.13: Debian ≤ 12, Ubuntu ≤ 24.04) accepts u: 32 bytes at most, a leading
// letter, and lower-case letters, digits and dashes only.
func userAddOK(u string) bool {
	if len(u) == 0 || len(u) > maxSMBUser {
		return false
	}
	c := u[0]
	if c < 'a' || c > 'z' {
		return false
	}
	for _, r := range u {
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// TestSMBNonWindowsHasNoUser is the access rule the rest of the SMB tests
// build on: only a Windows client gets a user; a Linux or macOS one mounts
// over NFS and gets none, and a Windows worker with no Hostinfo gets none,
// so a long name on those never reaches useradd.
func TestSMBNonWindowsHasNoUser(t *testing.T) {
	c := &Coordinator{secret: []byte("k")}
	s := &node{Name: "server-1"}
	for _, n := range []*node{
		{Name: "linux-1", Hostinfo: &tailcfg.Hostinfo{OS: "linux"}},
		{Name: "darwin-1", Hostinfo: &tailcfg.Hostinfo{OS: "darwin"}},
		{Name: "windows-no-hostinfo"},
	} {
		if got := c.smb(s, n); got != nil {
			t.Fatalf("smb(%s): got %+v, want nil", n.Name, got)
		}
	}
}

// TestSMBShortNameIsVerbatim is the unchanged path: a Windows worker whose
// "mp-"+name fits useradd's bound 32 keeps that name verbatim, and the
// password is the HMAC of "server/client" alone. Bug reports names 30+ break;
// this checks the names that never did still do not.
func TestSMBShortNameIsVerbatim(t *testing.T) {
	secret := []byte("harness-key")
	c := &Coordinator{secret: secret}
	cases := []struct {
		server, worker string
	}{
		{"server-1", "alex"},
		{"server-1", "box-2"},
		{"s", "n"},
		{"server-with-dashes", "windows-10"},
		// mp- + 29 = 32: the last length useradd accepts on shadow ≤ 4.13.
		{"srv", strings.Repeat("a", 29)},
	}
	for _, tc := range cases {
		s, n := &node{Name: tc.server}, win(tc.worker)
		got := c.smb(s, n)
		if got == nil {
			t.Fatalf("smb(%s,%s): nil", tc.server, tc.worker)
		}
		wantU := "mp-" + tc.worker
		if got.User != wantU {
			t.Errorf("smb(%s,%s).User = %q, want %q (no hash until len > %d)", tc.server, tc.worker, got.User, wantU, maxSMBUser)
		}
		if got.Password != hmacHex(secret, tc.server+"/"+tc.worker) {
			t.Errorf("smb(%s,%s).Password = %q, want HMAC of %q", tc.server, tc.worker, got.Password, tc.server+"/"+tc.worker)
		}
		if !userAddOK(got.User) {
			t.Errorf("smb(%s,%s).User = %q: not a name useradd accepts", tc.server, tc.worker, got.User)
		}
	}
}

// TestSMBLongNameIsBounded is the fix: a Windows worker whose name makes the
// SMB user exceed useradd's limit gets a stable, bounded name instead, so
// useradd never rejects it on any supported distro. The bug report names 30
// the breaking point (mp- + 30 = 33); this checks 30, 63 (a DNS label) and
// far past it.
func TestSMBLongNameIsBounded(t *testing.T) {
	secret := []byte("harness-key")
	c := &Coordinator{secret: secret}
	cases := []struct {
		server, worker string
	}{
		// mp- + 30 = 33: the first length useradd rejects on shadow ≤ 4.13.
		{"srv", strings.Repeat("a", 30)},
		// A DNS first label can be 63 chars.
		{"srv", strings.Repeat("b", 63)},
		// Past any DNS limit, as the uniqueness suffix -2/-3 falls there too.
		{"srv", strings.Repeat("c", 1000)},
		// A long server name alone does not trigger hashing; only u does.
		{"a-very-long-server-name-that-is-itself-thirty-chars", strings.Repeat("d", 30)},
	}
	for _, tc := range cases {
		s, n := &node{Name: tc.server}, win(tc.worker)
		got := c.smb(s, n)
		if got == nil {
			t.Fatalf("smb(%s,%s): nil", tc.server, tc.worker)
		}
		if got.User == "mp-"+tc.worker {
			t.Errorf("smb(%s,%s).User = %q: not bounded (verbatim worker name)", tc.server, tc.worker, got.User)
		}
		if len(got.User) > maxSMBUser {
			t.Errorf("smb(%s,%s).User = %q: len %d > %d, useradd rejects", tc.server, tc.worker, got.User, len(got.User), maxSMBUser)
		}
		if !userAddOK(got.User) {
			t.Errorf("smb(%s,%s).User = %q: not a name useradd accepts", tc.server, tc.worker, got.User)
		}
		// The bounded name is "mp-" plus the first 29 hex chars of the same
		// HMAC with a "user" tag, so it shares no bytes with the password and
		// the same client pairs hold it across restarts.
		wantU := "mp-" + hmacHex(secret, tc.server+"/"+tc.worker+"user")[:maxSMBUser-3]
		if got.User != wantU {
			t.Errorf("smb(%s,%s).User = %q, want %q", tc.server, tc.worker, got.User, wantU)
		}
	}
}

// TestSMBPasswordUnchanged is the no-regression check on the password: even
// for a long name, it stays the HMAC of "server/client" alone — the same
// value every release before this gave out — since a Windows worker caches
// the credential by server and an old install must keep mounting.
func TestSMBPasswordUnchanged(t *testing.T) {
	secret := []byte("harness-key")
	c := &Coordinator{secret: secret}
	for _, w := range []string{"short", strings.Repeat("a", 29), strings.Repeat("a", 30), strings.Repeat("a", 63), strings.Repeat("a", 1000)} {
		s, n := &node{Name: "server-1"}, win(w)
		got := c.smb(s, n)
		if got == nil {
			t.Fatalf("smb(server-1,%s): nil", w)
		}
		want := hmacHex(secret, "server-1/"+w)
		if got.Password != want {
			t.Errorf("smb(server-1,%s).Password = %q, want %q", w, got.Password, want)
		}
		if len(got.Password) != 64 {
			t.Errorf("smb(server-1,%s).Password len = %d, want 64 hex chars", w, len(got.Password))
		}
	}
}

// TestSMBSamePairIsStable is the determinism check: the same (server, worker)
// pair always yields the same SMB user and password, on every round, since
// reconcile reruns the derivation and useradd must not delete and recreate a
// still-live share's user.
func TestSMBSamePairIsStable(t *testing.T) {
	c := &Coordinator{secret: []byte("harness-key")}
	s, n := &node{Name: "server-1"}, win(strings.Repeat("a", 30))
	first := c.smb(s, n)
	for i := 0; i < 5; i++ {
		got := c.smb(s, n)
		if got.User != first.User || got.Password != first.Password {
			t.Fatalf("round %d: smb = %+v, want %+v (stable)", i, got, first)
		}
	}
}

// TestSMBDistinctPairsDiffer is the no-collision check: within one server,
// distinct Windows clients get distinct SMB users and passwords (the user is
// local to that server's /etc/passwd, so the same worker name on different
// servers may share a user with no conflict). Names that hit the hash path
// get a 29-hex-char (116-bit) tail, well past any realistic collision chance,
// but the test stays exact.
func TestSMBDistinctPairsDiffer(t *testing.T) {
	c := &Coordinator{secret: []byte("harness-key")}
	long := win(strings.Repeat("a", 30))
	clients := []*node{
		win("alex"),
		win("box-2"),
		long,
		win(strings.Repeat("b", 30)),
	}
	for _, s := range []string{"server-1", "server-2"} {
		seenU, seenP := map[string]bool{}, map[string]bool{}
		for _, n := range clients {
			got := c.smb(&node{Name: s}, n)
			if got == nil {
				t.Fatalf("smb(%s,%s): nil", s, n.Name)
			}
			if seenU[got.User] {
				t.Errorf("server %s: user %q reused by two clients", s, got.User)
			}
			seenU[got.User] = true
			if seenP[got.Password] {
				t.Errorf("server %s: password reused by two clients", s)
			}
			seenP[got.Password] = true
		}
	}
	// Same long-name Windows worker on two different servers hashes to
	// different users (the server name is in the HMAC), so even a Windows
	// worker that is a client of two servers signs in to each with its own
	// name on that server.
	a := c.smb(&node{Name: "server-a"}, long).User
	b := c.smb(&node{Name: "server-b"}, long).User
	if a == b {
		t.Errorf("same long-name worker, different servers hashed to the same user %q", a)
	}
	// Two different long-name workers on the same server hash to different
	// users, so useradd on that server never sees a duplicate.
	x := c.smb(&node{Name: "server-1"}, win(strings.Repeat("a", 30))).User
	y := c.smb(&node{Name: "server-1"}, win(strings.Repeat("b", 30))).User
	if x == y {
		t.Errorf("different long-name workers hashed to the same user %q", x)
	}
}

// TestSMBUserAlwaysUserAddOK sweeps boundaries: every worker-name length from
// 1 to 200 yields a user that useradd on every supported distro accepts — the
// property the bug broke at 30, and the fix restores for all of them.
func TestSMBUserAlwaysUserAddOK(t *testing.T) {
	c := &Coordinator{secret: []byte("harness-key")}
	s := &node{Name: "server-1"}
	for l := 1; l <= 200; l++ {
		n := win(strings.Repeat("a", l))
		got := c.smb(s, n)
		if got == nil {
			t.Fatalf("len %d: nil", l)
		}
		if !userAddOK(got.User) {
			t.Errorf("len %d: User %q is not a useradd-acceptable name", l, got.User)
		}
		// The boundary is exact: 29 chars are kept verbatim and 30+ hash.
		keep := l <= maxSMBUser-len("mp-")
		verbatim := got.User == "mp-"+strings.Repeat("a", l)
		if keep && !verbatim {
			t.Errorf("len %d: User %q should be verbatim, want mp-%s", l, got.User, strings.Repeat("a", l))
		}
		if !keep && verbatim {
			t.Errorf("len %d: User %q should be hashed (len %d > %d)", l, got.User, 3+l, maxSMBUser)
		}
	}
}

// TestSMBServerNameLengthDoesNotTriggerHash checks the bound is on the SMB
// user the worker derives, not on the server's name: a long server name with
// a short worker name keeps the worker verbatim, since the user is named by
// the worker alone (mp- + worker name), not the server.
func TestSMBServerNameLengthDoesNotTriggerHash(t *testing.T) {
	c := &Coordinator{secret: []byte("harness-key")}
	n := win("alex")
	for _, sl := range []int{1, 10, 29, 30, 63, 100, 1000} {
		s := &node{Name: strings.Repeat("s", sl)}
		got := c.smb(s, n)
		if got == nil {
			t.Fatalf("server len %d: nil", sl)
		}
		if got.User != "mp-alex" {
			t.Errorf("server len %d: User = %q, want mp-alex (server name does not figure in the user)", sl, got.User)
		}
		// But a long server name does change the password, since it is part
		// of the HMAC message.
		if got.Password != hmacHex(c.secret, strings.Repeat("s", sl)+"/alex") {
			t.Errorf("server len %d: Password mismatch", sl)
		}
	}
}
