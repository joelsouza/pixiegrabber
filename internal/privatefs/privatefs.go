// Package privatefs creates local files and directories with owner-only access.
//
// The policy is deliberately current-owner-only: it does not add Everyone,
// Users, Administrators, or System ACEs.
package privatefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
)

const (
	privateFileMode os.FileMode = 0600
	privateDirMode  os.FileMode = 0700
	tempNameBytes               = 16
	maxTempAttempts             = 100
)

var (
	errInvalidPattern = errors.New("invalid temporary name pattern")
	errLink           = errors.New("link is not allowed")
	errWrongType      = errors.New("path has the wrong type")
	errChanged        = errors.New("path changed during operation")
)

func validatePattern(pattern string) error {
	if strings.IndexByte(pattern, 0) >= 0 || strings.ContainsAny(pattern, `/\\`) {
		return errInvalidPattern
	}
	return nil
}

func validatePath(path string) error {
	if strings.IndexByte(path, 0) >= 0 {
		return errors.New("path contains NUL")
	}
	return nil
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	// Do not expose a caller-provided path from an os.PathError. Preserve only
	// the underlying error so callers can still use os.IsExist and os.IsNotExist.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return &os.PathError{Op: op, Err: err}
}

func randomTempComponent() (string, error) {
	var bytes [tempNameBytes]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func tempName(pattern, random string) string {
	if star := strings.LastIndexByte(pattern, '*'); star >= 0 {
		return pattern[:star] + random + pattern[star+1:]
	}
	return pattern + random
}
