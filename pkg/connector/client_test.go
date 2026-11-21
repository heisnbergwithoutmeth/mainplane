package connector

import (
	"errors"
	"fmt"
	"testing"
)

// TestUnreachablePassesFriendlyError: a pointer 404 mapped to a fresh,
// friendly message (as pointer.Find now returns) is neither a *url.Error nor
// a *net.OpError, so unreachable returns it unchanged -- the message a
// saved login whose harness has dropped off the pointer reaches the user.
func TestUnreachablePassesFriendlyError(t *testing.T) {
	err := fmt.Errorf("no harness is published for this key: the harness is gone, or it has not reached the pointer for 30 days")
	got := unreachable(err)
	if !errors.Is(got, err) {
		t.Fatalf("unreachable rewrote a friendly error: got %v", got)
	}
	if got.Error() != err.Error() {
		t.Fatalf("unreachable changed the message: got %q, want %q", got.Error(), err.Error())
	}
}
