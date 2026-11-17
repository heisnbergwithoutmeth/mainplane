package worker

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// asCall is one recorded call to as: the System32 program name and its
// args (the slice is a copy the caller cannot mutate).
type asCall struct {
	name string
	args []string
}

// fakeAs is a test double for the Windows as seam. It records every call,
// answers cmdkey /add (success unless failCmdkey is set, so a credential
// re-store can be forced to fail) and net use. net use with only "use"
// answers the mapping table (the production mappings() function parses it);
// net use with letter/remote adds the mapping, mutating the table so the
// mount's later mappings() call reports the new mapping; net use with
// "/delete" removes it.
type fakeAs struct {
	t          *testing.T
	calls      []asCall
	mappings   map[string]string
	failCmdkey bool
}

func (f *fakeAs) run(_ *user.User, name string, args ...string) (string, error) {
	f.t.Helper()
	rec := append([]string(nil), args...)
	f.calls = append(f.calls, asCall{name: name, args: rec})
	switch {
	case name == "cmdkey":
		if f.failCmdkey {
			return "logon failure", errors.New("system error 1326: logon failure")
		}
		// /list in unmap looks for "target=<host>".
		if len(args) > 0 && args[0] == "/list" {
			var b strings.Builder
			for _, letter := range slices.Sorted(maps.Keys(f.mappings)) {
				fmt.Fprintf(&b, "Target: %s\n", strings.TrimPrefix(f.mappings[letter], `\\`))
			}
			return b.String(), nil
		}
		return "", nil
	case name == "net" && len(args) == 1 && args[0] == "use":
		// mapping table request (mappings()).
		var b strings.Builder
		for _, letter := range slices.Sorted(maps.Keys(f.mappings)) {
			fmt.Fprintf(&b, "OK   %s    %s    Microsoft Windows Network\n", letter, f.mappings[letter])
		}
		return b.String(), nil
	case name == "net" && len(args) >= 3 && args[0] == "use" && slices.Contains(args, "/delete"):
		// net use <letter> /delete [/y] — unmap; reverse of the add.
		delete(f.mappings, args[1])
		return "", nil
	case name == "net" && len(args) == 4 && args[0] == "use":
		// net use <letter> <remote> /persistent:no — fresh mount succeeds.
		f.mappings[args[1]] = args[2]
		return "", nil
	}
	return "", nil
}

// cmdKeyAddCalls returns every recorded cmdkey /add:<host> call's args.
func (f *fakeAs) cmdKeyAddCalls() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c.name == "cmdkey" && len(c.args) > 0 && strings.HasPrefix(c.args[0], "/add:") {
			out = append(out, c.args)
		}
	}
	return out
}

// withFakes replaces the as, label, and reach seams with test doubles for
// the life of this test. nil label and a constant reach are enough for
// every mount() path; the test reads f.calls to inspect System32 calls.
func withFakes(t *testing.T, f *fakeAs, reachReachable bool) {
	t.Helper()
	oldAs, oldLabel, oldReach := as, label, reach
	t.Cleanup(func() { as, label, reach = oldAs, oldLabel, oldReach })
	as = f.run
	label = func(*user.User, string, string, string) error { return nil }
	reach = func(netip.Addr) bool { return reachReachable }
	// lettersFile is overridden in tests that call load() or save().
}

// smb is a small helper for tests: a new SMB credential.
func smb(u, p string) *SMB { return &SMB{User: u, Password: p} }

// TestMountStoresCredentialOnFirstMap verifies that a first-time mount
// stores its SMB user/password in Credential Manager (call recorded as
// cmdkey /add with the harness-derived user and password) and remembers
// them in ours[name] so the next round need not re-store. Regresses the
// hoisted cmdkey call: it must run exactly once for a fresh mount, not
// again from the default arm.
func TestMountStoresCredentialOnFirstMap(t *testing.T) {
	f := &fakeAs{t: t, mappings: map[string]string{}}
	withFakes(t, f, true)

	// A drive whose letter is already ours but has never been mapped and
	// whose credential has never been stored: o.User/o.Pass are empty, so
	// the new compare-and-apply block runs cmdkey once and the default arm
	// runs net use.
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net"},
	}
	at := map[string]string{}
	cred := smb("mp-worker", "secret")
	m := Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: cred}

	d := mount(m, ours, at, nil)
	if d.State != Mounted {
		t.Fatalf("state: got %q, want %q (err=%q)", d.State, Mounted, d.Error)
	}
	adds := f.cmdKeyAddCalls()
	if len(adds) != 1 {
		t.Fatalf("cmdkey /add: got %d calls, want 1; calls: %+v", len(adds), f.calls)
	}
	want := []string{"/add:server.example.net", "/user:" + cred.User, "/pass:" + cred.Password}
	if !slices.Equal(adds[0], want) {
		t.Fatalf("cmdkey args: got %v, want %v", adds[0], want)
	}
	if got := ours["test"]; got.User != cred.User || got.Pass != cred.Password {
		t.Fatalf("ours[test] cred: got user=%q pass=%q, want %q %q", got.User, got.Pass, cred.User, cred.Password)
	}
}

// TestMountReStoresCredentialOnUserPasswordChange is the headline test for
// the bug: an already-mapped drive whose frame's SMB user and password both
// changed (a worker rename under the * rule). The bug dropped the changed
// frame on the floor; the fix re-runs cmdkey /add:<host> with the new user
// and password and remembers them.
func TestMountReStoresCredentialOnUserPasswordChange(t *testing.T) {
	f := &fakeAs{t: t, mappings: map[string]string{}}
	withFakes(t, f, true)
	remote := `\\server.example.net\test`
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net", Mapped: true, User: "mp-old", Pass: "old-pw"},
	}
	at := map[string]string{"W:": remote}
	newCred := smb("mp-new", "new-pw")

	d := mount(Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: newCred}, ours, at, nil)
	if d.State != Mounted {
		t.Fatalf("state: got %q, want %q (err=%q)", d.State, Mounted, d.Error)
	}
	adds := f.cmdKeyAddCalls()
	if len(adds) != 1 {
		t.Fatalf("cmdkey /add: got %d calls, want 1; calls: %+v", len(adds), f.calls)
	}
	want := []string{"/add:server.example.net", "/user:" + newCred.User, "/pass:" + newCred.Password}
	if !slices.Equal(adds[0], want) {
		t.Fatalf("cmdkey args after rename: got %v, want %v", adds[0], want)
	}
	if got := ours["test"]; got.User != newCred.User || got.Pass != newCred.Password {
		t.Fatalf("ours[test] after re-store: user=%q pass=%q, want %q %q", got.User, got.Pass, newCred.User, newCred.Password)
	}
}

// TestMountReStoresOnUserOnlyChange verifies the user name alone differing is
// enough to re-store: a rename can change the user and leave the derived
// password unchanged, but the credential is per-user on the server side.
func TestMountReStoresOnUserOnlyChange(t *testing.T) {
	f := &fakeAs{t: t, mappings: map[string]string{}}
	withFakes(t, f, true)
	remote := `\\server.example.net\test`
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net", Mapped: true, User: "mp-old", Pass: "shared-pw"},
	}
	at := map[string]string{"W:": remote}

	if d := mount(Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: smb("mp-new", "shared-pw")}, ours, at, nil); d.State != Mounted {
		t.Fatalf("state: got %q, want %q (err=%q)", d.State, Mounted, d.Error)
	}
	if adds := f.cmdKeyAddCalls(); len(adds) != 1 {
		t.Fatalf("cmdkey /add on user-only change: got %d, want 1; calls: %+v", len(adds), f.calls)
	}
	if got := ours["test"]; got.User != "mp-new" || got.Pass != "shared-pw" {
		t.Fatalf("ours[test]: user=%q pass=%q, want mp-new shared-pw", got.User, got.Pass)
	}
}

// TestMountCredentialReStoreFailsLeavesOldCredential verifies the failure
// mode: if cmdkey /add errors (e.g. Credential Manager is locked), the
// drive reports Failed and ours[name].User/Pass are not updated, so the
// next round retries from the changed frame instead of silently carrying
// the old credential.
func TestMountCredentialReStoreFailsLeavesOldCredential(t *testing.T) {
	f := &fakeAs{t: t, mappings: map[string]string{"W:": `\\server.example.net\test`}, failCmdkey: true}
	withFakes(t, f, true)
	remote := `\\server.example.net\test`
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net", Mapped: true, User: "mp-old", Pass: "old-pw"},
	}
	at := map[string]string{"W:": remote}

	d := mount(Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: smb("mp-new", "new-pw")}, ours, at, nil)
	if d.State != Failed {
		t.Fatalf("state: got %q, want %q (err=%q)", d.State, Failed, d.Error)
	}
	if got := ours["test"]; got.User != "mp-old" || got.Pass != "old-pw" {
		t.Fatalf("ours[test] changed despite cmdkey failure: user=%q pass=%q, want mp-old old-pw", got.User, got.Pass)
	}
	if adds := f.cmdKeyAddCalls(); len(adds) != 1 {
		t.Fatalf("cmdkey /add before fail: got %d, want 1; calls: %+v", len(adds), f.calls)
	}
}

// TestMountReStoresCredentialWhileServerUnreachable verifies the credential
// is re-stored BEFORE the switch on mapping state, so an unreachable server
// (Waiting) does not skip the update: when the server later answers, the
// new credential is already in Credential Manager, and the live SMB
// session that re-authenticates after a break succeeds.
func TestMountReStoresCredentialWhileServerUnreachable(t *testing.T) {
	f := &fakeAs{t: t, mappings: map[string]string{}}
	withFakes(t, f, false) // server unreachable

	remote := `\\server.example.net\test`
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net", Mapped: true, User: "mp-old", Pass: "old-pw"},
	}
	at := map[string]string{"W:": remote}

	d := mount(Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: smb("mp-new", "new-pw")}, ours, at, nil)
	if d.State != Waiting {
		t.Fatalf("state: got %q, want %q (err=%q)", d.State, Waiting, d.Error)
	}
	if adds := f.cmdKeyAddCalls(); len(adds) != 1 {
		t.Fatalf("cmdkey /add while waiting: got %d, want 1; calls: %+v", len(adds), f.calls)
	}
	if got := ours["test"]; got.User != "mp-new" || got.Pass != "new-pw" {
		t.Fatalf("ours[test] while waiting: user=%q pass=%q, want mp-new new-pw", got.User, got.Pass)
	}
}

// TestMountPersistsCredentialAcrossRounds verifies the cache invariant: once
// ours[name].User/Pass match the frame, second and later rounds skip cmdkey
// entirely, the way smbSet on the Linux serving side skips smbpasswd.
func TestMountPersistsCredentialAcrossRounds(t *testing.T) {
	f := &fakeAs{t: t, mappings: map[string]string{"W:": `\\server.example.net\test`}}
	withFakes(t, f, true)
	remote := `\\server.example.net\test`
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net", Mapped: true, User: "mp-old", Pass: "old-pw"},
	}
	at := map[string]string{"W:": remote}
	m := Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: smb("mp-new", "new-pw")}

	if d := mount(m, ours, at, nil); d.State != Mounted {
		t.Fatalf("first round: state=%q err=%q", d.State, d.Error)
	}
	if n := len(f.cmdKeyAddCalls()); n != 1 {
		t.Fatalf("first round: cmdkey /add %d times, want 1; calls: %+v", n, f.calls)
	}
	// Second round, same frame: no cmdkey, drive still mounted.
	if d := mount(m, ours, at, nil); d.State != Mounted {
		t.Fatalf("second round: state=%q err=%q", d.State, d.Error)
	}
	if n := len(f.cmdKeyAddCalls()); n != 1 {
		t.Fatalf("second round: cmdkey /add count should stay at 1; got %d; calls: %+v", n, f.calls)
	}
}

// TestLettersFilePersistedAfterCredChange writes lettersFile via save() after
// a credential rotation, reads it back via load(), and verifies ours's
// User/Pass survived the round-trip — actually exercising the file's
// new User/Pass JSON fields.
func TestLettersFilePersistedAfterCredChange(t *testing.T) {
	old := lettersFile
	lettersFile = filepath.Join(t.TempDir(), "drives.json")
	t.Cleanup(func() { lettersFile = old })
	f := &fakeAs{t: t, mappings: map[string]string{"W:": `\\server.example.net\test`}}
	withFakes(t, f, true)
	ours := map[string]mapped{
		"test": {Letter: "W:", Host: "server.example.net", Mapped: true, User: "mp-old", Pass: "old-pw"},
	}
	at := map[string]string{"W:": `\\server.example.net\test`}
	m := Mount{Name: "test", Host: "server.example.net", Addr: netip.MustParseAddr("127.0.0.1"), SMB: smb("mp-new", "new-pw")}
	if d := mount(m, ours, at, nil); d.State != Mounted {
		t.Fatalf("state: %q (err=%q)", d.State, d.Error)
	}
	if err := save(ours); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded := load()
	if got := loaded["test"]; got.User != "mp-new" || got.Pass != "new-pw" {
		t.Fatalf("lettersFile round-trip: user=%q pass=%q, want mp-new new-pw", got.User, got.Pass)
	}
}
