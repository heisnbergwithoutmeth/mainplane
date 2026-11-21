package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mainplane-ai/mainplane/pkg/server"
)

func tmpFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeHandoverFile(t *testing.T, h handover) string {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return tmpFile(t, "handover.json", string(b))
}

// A user config file cannot set the install's handover-only fields. A
// top-level "key" (also "harness", "join") in a config the operator hands to
// install must stay out of the install's secrets: it is not the install's
// api key, and it must not flip the root process onto the handback path.
func TestConfigUserFileDoesNotPopulateHandoverFields(t *testing.T) {
	body := `{
		"http": "127.0.0.1:8080",
		"providers": {"openai": {"shape": "openai", "key": "sk-x"}},
		"no_pointer": true,
		"key": "attacker-secret",
		"harness": "attacker-harness",
		"join": "attacker-join"
	}`
	p := tmpFile(t, "user.json", body)
	h, fresh, quick, keep, file := config([]string{p})
	if fresh || keep || quick {
		t.Fatalf("user file: fresh=%v keep=%v quick=%v (the bug keeps the old config and skips printing)", fresh, keep, quick)
	}
	if file != p {
		t.Fatalf("user file: file=%q want %q", file, p)
	}
	if h.Key != "" || h.Harness != "" || h.Join != "" {
		t.Fatalf("user file leaked handover-only fields into the install: key=%q harness=%q join=%q", h.Key, h.Harness, h.Join)
	}
	if h.HTTP != "127.0.0.1:8080" || h.Providers["openai"].Key != "sk-x" || !h.NoPointer {
		t.Fatalf("user file's config not read: http=%q providers=%+v no_pointer=%v", h.HTTP, h.Providers, h.NoPointer)
	}
}

// A --handover file, the temp file the unelevated stage wrote, does populate
// the handover fields, so the install's api key, harness key encoding and
// join token travel back to the unelevated parent that prints them.
func TestConfigHandoverFilePopulatesHandoverFields(t *testing.T) {
	src := handover{
		Config:  server.Config{HTTP: "127.0.0.1:8080"},
		Key:     "K",
		Harness: "H",
		Join:    "J",
	}
	p := writeHandoverFile(t, src)
	h, fresh, quick, _, file := config([]string{"--handover", p})
	if fresh || quick {
		t.Fatalf("handover file: fresh=%v quick=%v (want false)", fresh, quick)
	}
	if file != p {
		t.Fatalf("handover file: file=%q want %q", file, p)
	}
	if h.Key != "K" || h.Harness != "H" || h.Join != "J" {
		t.Fatalf("handover file: key=%q harness=%q join=%q (an install's secrets must travel back)", h.Key, h.Harness, h.Join)
	}
	if h.HTTP != "127.0.0.1:8080" {
		t.Fatalf("handover file: http=%q", h.HTTP)
	}
}

// The wire between the unelevated and elevated stages, for a config-file
// install and a plain install: a user file with a top-level "key" never
// becomes the install's key (the bug), and a plain install's random key
// survives the round-trip so the unelevated parent can print it.
func TestInstallHandoverWireRoundTripConfigFileAndPlain(t *testing.T) {
	userBody := `{
		"providers": {"openai": {"shape": "openai", "key": "sk-x"}},
		"key": "attacker-secret"
	}`
	userFile := tmpFile(t, "user.json", userBody)
	h1, fresh1, _, keep1, _ := config([]string{userFile})
	if fresh1 || keep1 || h1.Key != "" {
		t.Fatalf("config-file stage 1: fresh=%v keep=%v h.Key=%q (a user 'key' must not be the install's)", fresh1, keep1, h1.Key)
	}
	if h1.Providers["openai"].Key != "sk-x" {
		t.Fatalf("config-file stage 1: provider key not read: %+v", h1.Providers)
	}
	temp1 := writeHandoverFile(t, h1)
	h2, fresh2, _, keep2, _ := config([]string{"--handover", temp1})
	if fresh2 || keep2 || h2.Key != "" {
		t.Fatalf("config-file stage 2: fresh=%v keep=%v h.Key=%q (the silent no-op; the attacker key would be registered)", fresh2, keep2, h2.Key)
	}
	if h2.Providers["openai"].Key != "sk-x" {
		t.Fatalf("config-file stage 2: provider key lost in round-trip: %+v", h2.Providers)
	}

	h3, fresh3, _, keep3, _ := config(nil)
	if !fresh3 || !keep3 || h3.Key == "" {
		t.Fatalf("plain stage 1: fresh=%v keep=%v h.Key=%q", fresh3, keep3, h3.Key)
	}
	temp2 := writeHandoverFile(t, h3)
	h4, fresh4, _, keep4, _ := config([]string{"--handover", "--keep", temp2})
	if fresh4 || !keep4 || h4.Key != h3.Key {
		t.Fatalf("plain stage 2: fresh=%v keep=%v h.Key=%q want %q (the install's key must survive the round-trip)", fresh4, keep4, h4.Key, h3.Key)
	}
}

// `sudo mainplane-server install <config.json>` with a top-level "key":
// keep is false (so server.Install applies the file) and the handback
// branch keep && !fresh is false (so the user's file is not overwritten and
// the join/login lines print).
func TestConfigSudoInstallConfigFileDoesNotHandback(t *testing.T) {
	body := `{
		"http": "127.0.0.1:8080",
		"providers": {"openai": {"shape": "openai", "key": "sk-x"}},
		"key": "attacker-chooses-this"
	}`
	p := tmpFile(t, "config.json", body)
	h, fresh, _, keep, file := config([]string{p})
	if fresh {
		t.Fatalf("sudo install config.json: fresh=true (would log in the CLI; only a plain install does)")
	}
	if keep {
		t.Fatalf("sudo install config.json: keep=true (the bug overwrites the user's file with handover JSON)")
	}
	if file != p {
		t.Fatalf("sudo install config.json: file=%q want %q", file, p)
	}
	if h.Key != "" {
		t.Fatalf("sudo install config.json: h.Key=%q (the attacker's key would be registered as 'default')", h.Key)
	}
	if keep && !fresh {
		t.Fatal("sudo install config.json: keep && !fresh is true (the handback branch would overwrite the user's file and skip printing)")
	}
}
