package chunked

import (
	"fmt"
	"io"
	"strings"

	"github.com/vbatts/tar-split/archive/tar"
	"go.podman.io/storage/pkg/chunked/internal/minimal"
)

// comparePathComponents compares two paths by splitting them into components
// and comparing component-by-component.  This matches the order produced by
// filesystem walks (depth-first with sorted directory entries), which differs
// from a flat string comparison when a directory name is a prefix of a sibling
// file name (e.g. "gnus/" sorts before "gnus-tut.txt" by component, but after
// it by flat string because '-' < '/').
func comparePathComponents(a, b string) int {
	aParts := strings.Split(a, "/")
	bParts := strings.Split(b, "/")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] < bParts[i] {
			return -1
		}
		if aParts[i] > bParts[i] {
			return 1
		}
	}
	if len(aParts) < len(bParts) {
		return -1
	}
	if len(aParts) > len(bParts) {
		return 1
	}
	return 0
}

// validateTOCEntriesOrder checks that the non-chunk entries in a v2 TOC are
// sorted by path components.  A canonical tar stream requires a deterministic
// order; receiving entries out of order means the image was not built correctly.
func validateTOCEntriesOrder(entries []minimal.FileMetadata) error {
	prevName := ""
	for i := range entries {
		if entries[i].Type == minimal.TypeChunk {
			continue
		}
		if comparePathComponents(entries[i].Name, prevName) < 0 {
			return fmt.Errorf("TOC entries are not sorted: %q appears after %q", entries[i].Name, prevName)
		}
		prevName = entries[i].Name
	}
	return nil
}

// tarSizeFromTOC computes the total uncompressed tar size from the TOC
// metadata alone, without needing tarsplit data.
//
// This works by constructing canonical tar headers and writing them (with
// zero-filled file content) through a real tar.Writer to a counting writer,
// ensuring all padding and end-of-archive markers are accounted for.
func tarSizeFromTOC(tocEntries []minimal.FileMetadata) (int64, error) {
	cw := &countingWriter{}
	tw := tar.NewWriter(cw)

	for i := range tocEntries {
		e := &tocEntries[i]
		if e.Type == minimal.TypeChunk {
			continue
		}

		hdr, err := minimal.FileMetadataToCanonicalTarHeader(e)
		if err != nil {
			return -1, err
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return -1, fmt.Errorf("writing header for %q: %w", e.Name, err)
		}

		if hdr.Size > 0 {
			if _, err := io.CopyN(tw, zeroReader{}, hdr.Size); err != nil {
				return -1, fmt.Errorf("writing zero content for %q: %w", e.Name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return -1, err
	}

	return cw.n, nil
}

// countingWriter counts bytes written to it without actually storing them.
type countingWriter struct {
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// zeroReader is an io.Reader that produces an endless stream of zeros.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
