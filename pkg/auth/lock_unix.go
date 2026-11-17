//go:build !windows

package auth

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// lockFile takes an advisory lock on f, exclusive when write is true and
// shared otherwise. The lock is a flock: the running harness and the root
// CLI are on this machine, so a local file lock reaches both. A process
// running as root that opens the lock file before the harness user (a CLI
// key command after an upgrade, or the install's first Add before the
// harness locks it) would make it root's and lock the harness out, so a
// root chowns f to the table's owner first; the harness, already that
// user, leaves it as its own.
func lockFile(f *os.File, tablePath string, write bool) error {
	if os.Geteuid() == 0 { // a root CLI hands the lock file to the harness user
		if info, err := os.Stat(tablePath); err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				_ = unix.Fchown(int(f.Fd()), int(stat.Uid), int(stat.Gid))
			}
		}
	}
	how := unix.LOCK_SH
	if write {
		how = unix.LOCK_EX
	}
	return unix.Flock(int(f.Fd()), how)
}
