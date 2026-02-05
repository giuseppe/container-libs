//go:build linux

package splitfdstream

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"

	"go.podman.io/storage/pkg/fileutils"
)

// SplitFDStreamIterator implements archive.TarEntryIterator for the
// splitfdstream format, allowing it to plug into archive.UnpackFromIterator.
type SplitFDStreamIterator struct {
	reader      *splitFDStreamReader
	currentFile *os.File // external FD for reflink
	currentData []byte   // inline content fallback
	prevFile    *os.File // previous external FD, closed on next Next()
}

// NewIterator creates a new SplitFDStreamIterator.
func NewIterator(stream io.Reader, fds []*os.File) *SplitFDStreamIterator {
	return &SplitFDStreamIterator{
		reader: newReader(stream, fds),
	}
}

// NewIteratorWithFDReceiver creates a SplitFDStreamIterator that receives FDs
// lazily via the provided callback (typically backed by SCM_RIGHTS over a Unix
// socket). Each call to the callback should return the next FD in stream order.
// FDs are automatically closed after each entry is processed.
func NewIteratorWithFDReceiver(stream io.Reader, recv func() (*os.File, error)) *SplitFDStreamIterator {
	return &SplitFDStreamIterator{
		reader: newReaderWithFDReceiver(stream, recv),
	}
}

// Next advances to the next entry and returns its tar header.
// For TypeReg entries with Size > 0, it pre-reads the content chunk
// (either an external FD reference or inline data) so that WriteContentTo
// can deliver it.
func (i *SplitFDStreamIterator) Next() (*tar.Header, error) {
	// Close the previous external FD to avoid accumulating open FDs
	if i.prevFile != nil {
		i.prevFile.Close()
		i.prevFile = nil
	}

	// Reset state from previous entry
	i.currentFile = nil
	i.currentData = nil

	// Read next inline chunk (must be a tar header)
	c, err := i.reader.nextChunk()
	if err != nil {
		return nil, err // includes io.EOF
	}

	if c.typ != chunkTypeInline {
		return nil, fmt.Errorf("expected inline chunk with tar header, got external chunk")
	}

	// Parse the tar header from the inline data
	header, err := parseTarHeader(c.data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tar header: %w", err)
	}

	// For regular files with content, pre-read the next chunk
	if (header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA) && header.Size > 0 {
		contentChunk, err := i.reader.nextChunk()
		if err != nil {
			return nil, fmt.Errorf("failed to read content chunk for %s: %w", header.Name, err)
		}

		switch contentChunk.typ {
		case chunkTypeExternal:
			i.currentFile = contentChunk.file
			i.prevFile = contentChunk.file // track for closing on next Next()
		case chunkTypeInline:
			i.currentData = contentChunk.data
		}
	}

	return header, nil
}

// WriteContentTo writes the current entry's file content to dst.
// If the content came from an external FD, it uses reflink for efficiency.
// If the content is inline data, it writes it directly.
func (i *SplitFDStreamIterator) WriteContentTo(dst *os.File) error {
	if i.currentFile != nil {
		// Seek source to beginning for reflink/copy
		if _, err := i.currentFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek source file: %w", err)
		}
		return fileutils.ReflinkOrCopy(i.currentFile, dst)
	}

	if i.currentData != nil {
		_, err := dst.Write(i.currentData)
		return err
	}

	return nil
}

// parseTarHeader parses a tar header from raw bytes.
func parseTarHeader(data []byte) (*tar.Header, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	header, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("failed to read tar header: %w", err)
	}
	return header, nil
}
