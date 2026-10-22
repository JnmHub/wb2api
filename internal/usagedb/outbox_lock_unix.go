//go:build !windows

package usagedb

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockOutbox(path string) (*os.File, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func unlockOutbox(f *os.File) error { return f.Close() }
