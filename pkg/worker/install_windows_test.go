package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// TestMain is also the test service. When the SCM starts this binary it
// runs as the service mainplaned: it reports Running (with AcceptStop and
// AcceptShutdown) once the milliseconds its first service arg names have
// passed, or at once when that is zero, and stops at the first stop or
// shutdown control. The delay puts the service in the START_PENDING,
// no-controls-accepted window the bug report describes, so stop and
// Uninstall -- which use the const name -- run against this very service.
func TestMain(m *testing.M) {
	if ok, _ := svc.IsWindowsService(); ok {
		if err := svc.Run(name, testService{}); err != nil {
			fmt.Fprintf(os.Stderr, "svc.Run: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

type testService struct{}

func (testService) Execute(args []string, reqs <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	if len(args) > 1 {
		if d, err := strconv.Atoi(args[1]); err == nil && d > 0 {
			time.Sleep(time.Duration(d) * time.Millisecond)
		}
	}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for r := range reqs {
		//exhaustive:ignore the service accepts stop and shutdown only
		switch r.Cmd {
		case svc.Interrogate:
			status <- r.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

// admin reports whether this process can create and start services.
func admin() bool {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_CREATE_SERVICE)
	if err != nil {
		return false
	}
	_ = windows.CloseServiceHandle(h)
	return true
}

// waitForState polls the service until its state is one of want, or until
// the timeout elapses (in which case it returns the last state and a nil
// error if it tried; tests then assert the state themselves).
func waitForState(s *mgr.Service, want ...svc.State) (svc.State, error) {
	st, err := s.Query()
	if err != nil {
		return 0, err
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		for _, w := range want {
			if st.State == w {
				return st.State, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return 0, err
		}
	}
	return st.State, nil
}

// killStop stops the service if it is not Stopped, swallows whatever the SCM
// returns -- a STOP_PENDING service rejects a second stop, and a delete-
// pending record rejects one too -- and waits for Stopped, so the record
// can be deleted.
func killStop(s *mgr.Service) {
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		_, _ = s.Control(svc.Stop)
		_, _ = waitForState(s, svc.Stopped)
	}
}

// gone reports whether the service named name has gone from the SCM. A
// delete-pending, stopped service's record is removed asynchronously once
// the last handle is closed, so this polls for a few seconds before
// deciding it survived.
func gone() bool {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		m, err := mgr.Connect()
		if err != nil {
			return false
		}
		s, err := m.OpenService(name)
		_ = m.Disconnect()
		if err != nil {
			return true
		}
		_ = s.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// install makes this binary the service mainplaned with the named delay arg
// and starts it. A service left from a failed run is best-effort deleted
// first. Cleanup stops and deletes the service even if the test fails.
func install(t *testing.T, delayMS int) *mgr.Service {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	if old, err := m.OpenService(name); err == nil {
		// Delete a leftover; if its record is marked for deletion,
		// Delete returns ERROR_SERVICE_MARKED_FOR_DELETE, which is
		// fine -- the record goes when the process stops.
		killStop(old)
		_ = old.Delete()
		_ = old.Close()
	}
	s, err := m.CreateService(name, exe, mgr.Config{
		ServiceType:    windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:      mgr.StartManual,
		ErrorControl:   mgr.ErrorNormal,
		BinaryPathName: exe,
		DisplayName:    "Mainplane worker test",
	}, strconv.Itoa(delayMS))
	if err != nil {
		_ = m.Disconnect()
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		killStop(s)
		_ = s.Delete()
		_ = s.Close()
		_ = m.Disconnect()
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// vars points the package vars Uninstall touches at a temp directory, then
// restores them when the test ends. stateDir holds the join token Uninstall
// removes, Bin/wintunDLL/cliDir the files, logPath the log.
func vars(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	set := func(dst *string, val string) {
		old := *dst
		*dst = val
		t.Cleanup(func() { *dst = old })
	}
	set(&stateDir, dir)
	set(&Bin, filepath.Join(dir, "bin", "mainplane.exe"))
	set(&wintunDLL, filepath.Join(filepath.Dir(Bin), "wintun.dll"))
	set(&cliDir, filepath.Join(dir, "cli"))
	set(&logPath, filepath.Join(dir, "mainplaned.log"))
}

// TestStopPollsStartPending is the bug: a service still START_PENDING
// accepts no controls, so a stop sent to it is rejected. stop polls until
// the service reaches Running and then stops it, instead of returning the
// rejected-control error. Without the fix, stop returns
// ERROR_INVALID_SERVICE_CONTROL before opening the process, leaving the
// service running and Uninstall -- which calls stop below -- to fail too.
func TestStopPollsStartPending(t *testing.T) {
	if !admin() {
		t.Skip("requires admin to manage services")
	}
	s := install(t, 2000) // 2 s START_PENDING window
	if st, err := s.Query(); err == nil && st.State != svc.StartPending {
		t.Logf("initial state %v, not StartPending", st.State)
	}
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("stop returned after %v, expected to poll through the START_PENDING window", elapsed)
	}
	if st, _ := waitForState(s, svc.Stopped); st != svc.Stopped {
		t.Fatalf("service still %v after stop", st)
	}
}

// TestStopRunning stops a service that's already Running, the path that
// worked before the fix and must still work: query Running, send Stop, wait
// for the process to end.
func TestStopRunning(t *testing.T) {
	if !admin() {
		t.Skip("requires admin to manage services")
	}
	s := install(t, 0) // reports Running at once
	if st, _ := waitForState(s, svc.Running); st != svc.Running {
		t.Fatalf("service did not reach Running: %v", st)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if st, _ := waitForState(s, svc.Stopped); st != svc.Stopped {
		t.Fatalf("service still %v after stop", st)
	}
}

// TestUninstallDuringStartPending is the bug report: Uninstall deletes the
// service first, then sends the drop control. While the service is
// START_PENDING the control is rejected; with the fix the rejection is
// tolerated, so Uninstall proceeds to stop (which polls the START_PENDING
// service to Running and stops it), and the cleanup -- the state directory
// with the join token, the binary, the PATH entries -- runs. Without the
// fix Uninstall returns the rejected-control error after Delete has marked
// the record for deletion, and a retry wedges on
// ERROR_SERVICE_MARKED_FOR_DELETE before reaching stop or the cleanup.
func TestUninstallDuringStartPending(t *testing.T) {
	if !admin() {
		t.Skip("requires admin to manage services")
	}
	vars(t)
	s := install(t, 2000) // 2 s START_PENDING window
	if st, err := s.Query(); err == nil && st.State != svc.StartPending {
		t.Logf("initial state %v, not StartPending", st.State)
	}
	if err := os.MkdirAll(filepath.Dir(Bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "join"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	// Uninstall drops s's underlying record; reopen must say so. The SCM
	// removes a delete-pending, stopped service's record asynchronously
	// once the last handle is closed, so poll for a moment.
	if !gone() {
		t.Fatal("service survived Uninstall")
	}
	if _, err := os.Stat(stateDir); err == nil {
		t.Fatal("stateDir survived Uninstall")
	}
}

// TestUninstallRetryAfterMarkedForDelete is the retry wedge: a service
// already marked for deletion rejects a second Delete with
// ERROR_SERVICE_MARKED_FOR_DELETE; with the fix Uninstall tolerates it and
// proceeds to stop and cleanup. Without the fix a retry of Uninstall
// returned ERROR_SERVICE_MARKED_FOR_DELETE before reaching stop, leaving
// the worker running on the mesh and the join token and the mesh keys in
// place for the rest of the boot.
func TestUninstallRetryAfterMarkedForDelete(t *testing.T) {
	if !admin() {
		t.Skip("requires admin to manage services")
	}
	vars(t)
	s := install(t, 0) // Running at once
	if st, _ := waitForState(s, svc.Running); st != svc.Running {
		t.Fatalf("service did not reach Running: %v", st)
	}
	// The first Delete marks the record; the process is still Running, so
	// the record stays until the service stops, and the retry of Uninstall
	// finds it.
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		t.Fatalf("pre-Uninstall Delete: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(Bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "join"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !gone() {
		t.Fatal("service survived Uninstall")
	}
	if _, err := os.Stat(stateDir); err == nil {
		t.Fatal("stateDir survived Uninstall")
	}
}
