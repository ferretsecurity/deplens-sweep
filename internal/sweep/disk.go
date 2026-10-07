package sweep

import (
	"errors"
	"syscall"
)

const defaultMinFreeBytes uint64 = 10 * 1024 * 1024 * 1024

func cloneSpaceAvailable(parent string, reserve uint64) (bool, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(parent, &stats); err != nil || stats.Bsize <= 0 {
		return false, errors.New("cannot inspect free space for temporary checkouts")
	}
	return stats.Bavail*uint64(stats.Bsize) >= reserve, nil
}
