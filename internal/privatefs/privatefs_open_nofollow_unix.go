//go:build linux || darwin

package privatefs

import (
	"os"
	"syscall"
)

func openPrivate(path string, _ bool) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
