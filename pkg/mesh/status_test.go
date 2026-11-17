//go:build !windows

package mesh

import (
	"bufio"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// withSocket points the package's status socket at a fresh path for the test
// and restores the real one after, so Joined speaks to the fake, never to a
// worker that may really run on this machine.
func withSocket(t *testing.T) {
	t.Helper()
	old := socket
	socket = filepath.Join(t.TempDir(), "mainplaned.sock")
	t.Cleanup(func() { socket = old })
}

// fake listens at socket, reads the one line each client writes, writes reply
// (nothing when reply is empty, as the daemon's "joined" verb does before the
// first netmap), and closes. Closing ln stops the goroutine.
func fake(t *testing.T, reply string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = bufio.NewReader(c).ReadString('\n') // ask writes the line first
				if reply != "" {
					_, _ = io.WriteString(c, reply)
				}
			}(c)
		}
	}()
	return ln
}

// TestJoinedNoDaemon: no socket answers, so ask wraps the dial failure in
// ErrNoWorker -- the signal that no worker is installed at all, which a
// --no-admin install may skip over since uninstallAdmin is a no-op with no
// worker on disk.
func TestJoinedNoDaemon(t *testing.T) {
	withSocket(t) // a fresh path with nothing listening
	_, _, err := Joined()
	if err == nil {
		t.Fatal("no daemon: want error, got nil")
	}
	if !errors.Is(err, ErrNoWorker) {
		t.Fatalf("no daemon: want ErrNoWorker, got %v", err)
	}
}

// TestJoinedEmptyAnswerIsNotNoWorker: the daemon serves its control socket as
// soon as it starts but answers "joined" only after the first netmap; until
// then it writes nothing. The client must turn that empty answer into an error
// that is NOT ErrNoWorker, so callers can tell "no daemon" from "daemon present
// but not yet joined" -- the distinction the --no-admin install relies on to
// remove a leftover admin worker instead of skipping it and reporting success.
func TestJoinedEmptyAnswerIsNotNoWorker(t *testing.T) {
	withSocket(t)
	ln := fake(t, "")
	defer func() { _ = ln.Close() }()
	h, name, err := Joined()
	if err == nil {
		t.Fatalf("empty answer: want error, got %q %q", h, name)
	}
	if errors.Is(err, ErrNoWorker) {
		t.Fatalf("empty answer: want a non-ErrNoWorker error (daemon is up), got ErrNoWorker: %v", err)
	}
	if h != "" || name != "" {
		t.Fatalf("empty answer: want zero values, got %q %q", h, name)
	}
}

// TestJoinedParse: a live netmap answer is "harness name\n", which Joined
// splits into the harness key and the worker's name there.
func TestJoinedParse(t *testing.T) {
	withSocket(t)
	ln := fake(t, "KEYKEY worker-name\n")
	defer func() { _ = ln.Close() }()
	h, name, err := Joined()
	if err != nil {
		t.Fatalf("joined: want nil error, got %v", err)
	}
	if h != "KEYKEY" || name != "worker-name" {
		t.Fatalf("joined: want KEYKEY worker-name, got %q %q", h, name)
	}
}
