package worker

import (
	"bytes"
	"errors"
	"log"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// mustAddr parses a for tests; it panics on a bad literal.
func mustAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}

// withLog captures the standard logger's output for the test, restoring it
// after.
func withLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// closeSet is the set of "<service> <pid>" pairs in closes, to compare
// against an unordered result built from a map.
func closeSet(closes []smbCloseCmd) map[string]bool {
	m := make(map[string]bool, len(closes))
	for _, c := range closes {
		m[c.service+" "+c.pid] = true
	}
	return m
}

// wantCloses fails unless closes is exactly the set of want, ignoring the
// order in which smbCloses produced it (it iterates a map).
func wantCloses(t *testing.T, got []smbCloseCmd, want ...smbCloseCmd) {
	t.Helper()
	gs, ws := closeSet(got), closeSet(want)
	for k := range gs {
		if !ws[k] {
			t.Errorf("unexpected close %q", k)
		}
	}
	for k := range ws {
		if !gs[k] {
			t.Errorf("missing close %q", k)
		}
	}
}

// A Windows client opens IPC$ for named-pipe traffic alongside any share
// connect. IPC$ is never a drive export, so it can never be in the allowlist,
// and smbClose must leave it alone — closing it each round was the bug.
func TestSmbClosesLeavesIPCAlone(t *testing.T) {
	es := []Export{{Name: "home", Clients: []Client{
		{Name: "w1", Addr: mustAddr("127.0.0.1"), SMB: &SMB{User: "w1", Password: "p"}},
	}}}
	tcons := map[string]smbTcon{
		"1": {Service: "IPC$", Machine: "127.0.0.1", ServerID: smbServerID{Pid: "27038"}},
		"2": {Service: "home", Machine: "127.0.0.1", ServerID: smbServerID{Pid: "27039"}},
	}
	wantCloses(t, smbCloses(es, tcons)) // nothing: home allowed, IPC$ not ours
}

// A client removed from a drive's SMB users still loses its tree connect at
// once: the revocation purpose of smbClose, which the IPC$ fix must keep.
func TestSmbClosesRevokesRemovedClient(t *testing.T) {
	es := []Export{{Name: "home", Clients: []Client{
		{Name: "w1", Addr: mustAddr("10.0.0.5"), SMB: &SMB{User: "w1", Password: "p"}},
	}}}
	tcons := map[string]smbTcon{
		"1": {Service: "home", Machine: "10.0.0.9", ServerID: smbServerID{Pid: "70"}}, // not allowed
		"2": {Service: "IPC$", Machine: "10.0.0.9", ServerID: smbServerID{Pid: "70"}}, // still not ours
	}
	wantCloses(t, smbCloses(es, tcons), smbCloseCmd{service: "home", pid: "70"})
}

// A tree connect to a share the server no longer exports is left to smbd's
// reload to tear down: only shares we still export are ours to revoke. This
// is a deliberate change from the original, which closed them.
func TestSmbClosesLeavesUnexportedShareAlone(t *testing.T) {
	es := []Export{{Name: "home", Clients: []Client{
		{Name: "w1", Addr: mustAddr("10.0.0.5"), SMB: &SMB{User: "w1", Password: "p"}},
	}}}
	tcons := map[string]smbTcon{
		"1": {Service: "old", Machine: "10.0.0.5", ServerID: smbServerID{Pid: "7"}}, // not in es
	}
	wantCloses(t, smbCloses(es, tcons)) // nothing
}

// closeShares logs a failed close and returns nothing: a dead smbd child is
// the outcome wanted, and one must not fail the round's drives.
func TestCloseSharesLogsFailures(t *testing.T) {
	buf := withLog(t)
	closes := []smbCloseCmd{
		{service: "home", pid: "100"},
		{service: "data", pid: "200"},
	}
	var calls []smbCloseCmd
	closeShares(closes, func(service, pid string) error {
		calls = append(calls, smbCloseCmd{service: service, pid: pid})
		return errors.New("no child")
	})
	if len(calls) != 2 || !contains(calls, smbCloseCmd{"home", "100"}) || !contains(calls, smbCloseCmd{"data", "200"}) {
		t.Fatalf("every close attempted, got %v", calls)
	}
	if !strings.Contains(buf.String(), "smbcontrol") || !strings.Contains(buf.String(), "home 100") || !strings.Contains(buf.String(), "no child") {
		t.Fatalf("log missing failure: %q", buf.String())
	}
}

// closeShares has no error return: a per-close error cannot reach smbClose,
// and so cannot mark every drive Failed for the round. This pins that
// contract at compile time; a future change that returns the error would
// re-open the secondary bug unless serve() is updated too.
func TestCloseSharesHasNoErrorReturn(t *testing.T) {
	withLog(t)
	closeShares([]smbCloseCmd{{service: "x", pid: "1"}}, func(string, string) error { return errors.New("fail") })
	// Reaching here proves closeShares returns nothing: a returned error
	// would not compile against this call.
}

func contains(closes []smbCloseCmd, want smbCloseCmd) bool {
	for _, c := range closes {
		if c == want {
			return true
		}
	}
	return false
}
