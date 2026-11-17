package mesh

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/control/controlclient"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnlocal/ipnlocaltest"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// pollWait bounds the rig's wait for the spy's cc counter to reach an
// expected value; it matches the apps and harness tests' own wait.
const pollWait = 10 * time.Second

// settleWait is the final settle the test waits after Close and mock
// follow exit, before the assertion. The fork's Shutdown awaits its
// tracked goroutines via awaitNoGoroutinesInTest; the small extra wait
// bullet-proofs the assertion across subsystems that race on a racy run.
const settleWait = 200 * time.Millisecond

// bugWait bounds how long the test waits for the bug signature (Close
// returning without the fix, or cc#1's first Shutdown call) before
// switching to the fixed branch. With the fix, Close parks at <-m.follows
// and neither happens, so this is the test's wait on the fix path.
const bugWait = time.Second

// firstStartWait bounds the rig's wait for mock follow to offer its
// per-iteration gate, after a tick; it is also the rig's wait for lb.Start
// to land a control client in the spy on the first cycle.
const firstStartWait = 5 * time.Second

// spyCC is a controlclient.Client stand-in for the mesh's follow loop's
// control plane: every Start creates one through the backend's forTest
// hook, and Shutdown counts each call. A control client the generator
// produced but never shut down is the resurrection the fix prevents; the
// fork latches the backend's Shutdown so a leaked one keeps map-polling
// the harness until the process exits. cc#1 also closes rig.cc1Shut on
// its first Shutdown, so the test synchronizes the race on Shutdown's
// post-snapshot step rather than on race-y wall clock timing.
type spyCC struct {
	id            int32
	shutdownCalls atomic.Int32
	onShut        func() // optional; called on the first Shutdown call only.
}

func (c *spyCC) Shutdown() {
	n := c.shutdownCalls.Add(1)
	if n == 1 && c.onShut != nil {
		c.onShut()
	}
}
func (c *spyCC) Login(controlclient.LoginFlags)     {}
func (c *spyCC) Logout(context.Context) error       { return nil }
func (c *spyCC) SetPaused(bool)                     {}
func (c *spyCC) AuthCantContinue() bool             { return false }
func (c *spyCC) SetHostinfo(*tailcfg.Hostinfo)      {}
func (c *spyCC) SetNetInfo(*tailcfg.NetInfo)        {}
func (c *spyCC) SetTKAHead(string)                  {}
func (c *spyCC) UpdateEndpoints([]tailcfg.Endpoint) {}
func (c *spyCC) SetDiscoPublicKey(key.DiscoPublic)  {}
func (c *spyCC) SetIPForwardingBroken(bool)         {}
func (c *spyCC) ClientID() int64                    { return int64(c.id) }

// rig holds the bits of a Mesh built without going through Up: a real
// LocalBackend (no TUN thanks to ipnlocaltest.NewBackend), a mock follow
// loop shaped like the production one, and the channels Close drains.
// Up builds the same struct with a TUN and an engine; the test does not
// need them to exercise the drain contract.
type rig struct {
	t         *testing.T
	lb        *ipnlocal.LocalBackend
	m         *Mesh
	createdMu sync.Mutex
	created   []*spyCC
	counter   atomic.Int32

	// mockFollow gates:
	tick    chan struct{}      // mockFollow reads a tick from its top select.
	gates   chan chan struct{} // mockFollow offers a per-iteration gate; the test closes one to release lb.Start.
	cc1Shut chan struct{}      // closed on the first Shutdown call of cc#1.
}

func newRig(t *testing.T) *rig {
	t.Helper()
	// Matches mesh.Up: the coordinator's WebSocket upgrades use these.
	t.Setenv("TS_CONTROL_WEBSOCKET", "1")
	t.Setenv("TS_DEBUG_DERP_WS_CLIENT", "1")
	lb := ipnlocaltest.NewBackend(t)
	r := &rig{
		t:       t,
		lb:      lb,
		tick:    make(chan struct{}, 1),
		gates:   make(chan chan struct{}, 1),
		cc1Shut: make(chan struct{}),
	}

	var cc1ShutOnce sync.Once
	lb.ForTest().SetControlClientGetter(func(opts controlclient.Options) (controlclient.Client, error) {
		id := r.counter.Add(1)
		c := &spyCC{id: id}
		if id == 1 {
			c.onShut = func() { cc1ShutOnce.Do(func() { close(r.cc1Shut) }) }
		}
		r.createdMu.Lock()
		r.created = append(r.created, c)
		r.createdMu.Unlock()
		return c, nil
	})

	ctx, unwatch := context.WithCancel(context.Background())
	r.m = &Mesh{
		lb:      lb,
		unwatch: unwatch,
		watched: make(chan struct{}),
		follows: make(chan struct{}),
		removed: make(chan struct{}),
	}
	// Close drains <-m.watched before leave(); the production watcher
	// closes it from its WatchNotifications defer, and the test does not
	// stand up a watcher, so close it now.
	close(r.m.watched)
	go r.mockFollow(ctx)
	return r
}

// mockFollow mirrors the production follow loop's cancellation shape:
// a top-level select that exits on ctx.Done, and an uncancellable lb.Start
// immediately after. Each iteration offers its gate to the test through
// r.gates, then blocks on that gate; the test alone decides when each
// lb.Start runs against a concurrently running Close, which makes the
// resurrection race deterministic instead of relying on tick timing.
//
// The gate wait deliberately does NOT exit on ctx.Done: that would defeat
// the gate's purpose and, more importantly, would not mirror the loop the
// bug lives in -- the production follow's lb.Start after the top select is
// uncancellable, and the resurrection happens when a ctx cancel that
// arrived after the select still doesn't stop lb.Start.
func (r *rig) mockFollow(ctx context.Context) {
	defer close(r.m.follows)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.tick:
		}
		gate := make(chan struct{})
		select {
		case r.gates <- gate:
		case <-ctx.Done():
			return
		}
		<-gate
		p := ipn.NewPrefs()
		p.ControlURL, p.Hostname, p.WantRunning = "http://test.invalid", "test", true
		p.AutoUpdate.Check = false
		_ = r.lb.Start(ipn.Options{UpdatePrefs: p})
	}
}

func (r *rig) count() int {
	r.createdMu.Lock()
	defer r.createdMu.Unlock()
	return len(r.created)
}

func (r *rig) waitCount(want int) {
	r.t.Helper()
	for end := time.Now().Add(pollWait); r.count() != want; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			r.t.Fatalf("want %d control clients in play; got %d", want, r.count())
		}
	}
}

func (r *rig) nextGate() chan struct{} {
	r.t.Helper()
	select {
	case g := <-r.gates:
		return g
	case <-time.After(firstStartWait):
		r.t.Fatal("mock follow did not offer its next gate in time")
		return nil
	}
}

func (r *rig) assertAllShutdown() {
	r.t.Helper()
	r.createdMu.Lock()
	defer r.createdMu.Unlock()
	if len(r.created) == 0 {
		r.t.Fatal("no control clients were created; rig setup is wrong")
	}
	for _, cc := range r.created {
		if got := cc.shutdownCalls.Load(); got == 0 {
			r.t.Errorf("control client #%d was created but never shut down: a follow tick called lb.Start after Shutdown and installed a fresh control client the backend's latch'd Shutdown can never stop", cc.id)
		}
	}
}

// TestMeshCloseDrainsFollowBeforeLeave verifies that Mesh.Close waits for
// follow to end before leave calls Shutdown. Without the wait, a follow
// tick already past its uncancellable top select calls lb.Start after
// Shutdown, which installs a brand-new controlclient nothing stops; its
// map-poll keeps knocking at the harness until the process exits.
func TestMeshCloseDrainsFollowBeforeLeave(t *testing.T) {
	r := newRig(t)

	// First cycle: produce cc#1 and let mock follow park back at the top of its loop.
	r.tick <- struct{}{}
	close(r.nextGate())
	r.waitCount(1)

	// Second cycle: hold mock follow at the per-iteration gate, past its
	// top select and about to call the uncancellable lb.Start #2, so
	// Close's Shutdown and lb.Start #2 line up in the bug's worst order.
	r.tick <- struct{}{}
	gate2 := r.nextGate()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.m.Close()
	}()

	// Wait for the bug's signature: either Close returned (Shut down cc#1
	// and finished), or cc#1's first Shutdown landed (Shutdown snapshotted
	// cc#1, released b.mu, and is calling cc#1.Shutdown outside the lock).
	// bugWait bounds the wait; with the fix, neither happens and Close is
	// parked at <-m.follows.
	select {
	case <-done:
	case <-r.cc1Shut:
	case <-time.After(bugWait):
	}

	// Release the gate: lb.Start #2 creates cc#2. Without the fix, cc#2
	// lands after Shutdown latched shutdownCalled=true, so nothing can
	// shut it down — the resurrected map-poll. With the fix, Close's
	// Shutdown runs after follow exits, so it takes the new cc#2 down.
	close(gate2)

	// Wait for Close to return (it already may have) and for mock follow
	// to fully exit. Whatever Close returned as error is bookkeeping:
	// leave's sync.Once body runs Shutdown first and hosts("") second, so
	// Shutdown ran regardless of a hosts-file write permission failure.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("m.Close did not return")
	}
	select {
	case <-r.m.follows:
	case <-time.After(5 * time.Second):
		t.Fatal("mock follow did not exit")
	}

	time.Sleep(settleWait)
	r.assertAllShutdown()
}

// TestMeshRemovalPathDrainsFollowBeforeLeave mirrors the harness-removal
// tear-down the manager writes a Removed anchor for (the watcher's "if
// removed" branch of Up's watcher goroutine, which calls harnessRemoved).
// The worker process keeps running after this returns, so a resurrected
// control client on this path keeps map-polling the harness forever.
func TestMeshRemovalPathDrainsFollowBeforeLeave(t *testing.T) {
	r := newRig(t)

	// First cycle: produce cc#1.
	r.tick <- struct{}{}
	close(r.nextGate())
	r.waitCount(1)

	// Second cycle: hold mock follow at its gate, past its top select.
	r.tick <- struct{}{}
	gate2 := r.nextGate()

	dir := t.TempDir()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.m.harnessRemoved(dir)
	}()

	// Same wait as Close; harnessRemoved runs the same drain-then-leave,
	// so the bug signature is the same.
	select {
	case <-done:
	case <-r.cc1Shut:
	case <-time.After(bugWait):
	}

	close(gate2)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("removal teardown did not return")
	}
	select {
	case <-r.m.removed:
	case <-time.After(5 * time.Second):
		t.Fatal("m.removed was not closed")
	}
	select {
	case <-r.m.follows:
	case <-time.After(5 * time.Second):
		t.Fatal("mock follow did not exit")
	}

	if _, err := os.Stat(filepath.Join(dir, Removed)); err != nil {
		t.Errorf("harnessRemoved did not write its Removed anchor: %v", err)
	}

	time.Sleep(settleWait)
	r.assertAllShutdown()
}
