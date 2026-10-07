package sweep

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func lockResults(root string) (*os.File, string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, "", errors.New("cannot create results directory")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", errors.New("cannot resolve results directory")
	}
	file, err := os.OpenFile(filepath.Join(canonical, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, "", errors.New("cannot open results directory lock")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, "", errors.New("results directory lock is not a regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, "", errors.New("results directory is locked by another invocation")
		}
		return nil, "", errors.New("cannot lock results directory")
	}
	// Leave the inode in place. Removing a released lock creates a race with a
	// process that already opened that inode. Close also releases locks after a crash.
	return file, canonical, nil
}
