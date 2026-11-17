//go:build windows

package auth

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an advisory lock on f, exclusive when write is true and
// shared otherwise. The lock is a LockFileEx: the running harness
// (LocalSystem) and the root CLI (the operator, elevated) are on this
// machine, and the table's directory's ACL -- which prepare() leaves each
// child to inherit -- gives both full access to the lock file, so neither
// needs chown.
func lockFile(f *os.File, _ string, write bool) error {
	var over windows.Overlapped
	var flags uint32
	if write {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &over)
}
