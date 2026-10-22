//go:build windows

package usagedb

import (
	"golang.org/x/sys/windows"
	"os"
)

func lockOutbox(path string) (*os.File, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	var o windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o)
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func unlockOutbox(f *os.File) error { return f.Close() }
