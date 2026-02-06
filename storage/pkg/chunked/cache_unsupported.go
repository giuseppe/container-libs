//go:build !linux

package chunked

import (
	"errors"
	"os"

	storage "go.podman.io/storage"
)

var errUnsupported = errors.New("FindFileByDigest is not supported on this platform")

// FileByDigestResult contains the result of a file lookup by digest.
type FileByDigestResult struct {
	File   *os.File
	Offset int64
	Size   int64
}

// FindFileByDigest is not supported on non-Linux platforms.
func FindFileByDigest(store storage.Store, digest string) (*FileByDigestResult, error) {
	return nil, errUnsupported
}
