package storage

import (
	"io"

	"go.podman.io/storage/pkg/archive"
)

// newCanonicalTarFilter wraps a tar stream so that every header is
// canonicalized before it is re-emitted. File content passes through
// unchanged.
func newCanonicalTarFilter(src io.ReadCloser) io.ReadCloser {
	return archive.NewCanonicalTarFilter(src)
}
