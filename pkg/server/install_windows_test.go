package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestResetChildACLsRefusesJunction plants a directory junction under Dir and
// checks resetChildACLs refuses it before SetNamedSecurityInfo lands on the
// junction's target. A directory junction needs no symlink privilege, so any
// local user could plant one; on Go >= 1.23 it reports fs.ModeIrregular, so a
// ModeSymlink check alone (the Unix guard) would miss it. The harness runs as
// LocalSystem and writes session state under Dir\sessions, so a planted
// sessions junction would otherwise write state through to an attacker's
// target.
func TestResetChildACLsRefusesJunction(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// The junction's name is sessions, the path the harness writes state to.
	link := filepath.Join(dir, "sessions")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	err := resetChildACLs(dir)
	if err == nil {
		t.Fatal("resetChildACLs: want refusal of the junction, got nil")
	}
	if !strings.Contains(err.Error(), "sessions") {
		t.Fatalf("resetChildACLs: error %q does not name the junction", err)
	}
	if !strings.Contains(err.Error(), "link or reparse point") {
		t.Fatalf("resetChildACLs: error %q does not say link or reparse point", err)
	}
}

// TestResetChildACLsAcceptsPlainChildren is the regression: with only plain
// files and directories under Dir, resetChildACLs resets their DACLs and
// returns nil, so a real install with no planted link behaves as before.
func TestResetChildACLsAcceptsPlainChildren(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetChildACLs(dir); err != nil {
		t.Fatalf("resetChildACLs: want nil for plain children, got %v", err)
	}
}
