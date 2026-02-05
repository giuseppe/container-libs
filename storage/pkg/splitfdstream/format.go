package splitfdstream

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// SplitFDStreamWriter writes data in the split FD stream format.
// The format uses signed 64-bit little-endian prefixes:
// - Negative prefix: abs(prefix) bytes of inline data follow
// - Non-negative prefix: Reference to external file descriptor at index prefix
type SplitFDStreamWriter struct {
	writer io.Writer
}

// NewWriter creates a new SplitFDStreamWriter.
func NewWriter(w io.Writer) *SplitFDStreamWriter {
	return &SplitFDStreamWriter{writer: w}
}

// WriteInline writes inline data with a negative prefix indicating the data length.
func (w *SplitFDStreamWriter) WriteInline(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("cannot write empty inline data")
	}

	// Write negative prefix indicating inline data length
	prefix := int64(-len(data))
	if err := binary.Write(w.writer, binary.LittleEndian, prefix); err != nil {
		return fmt.Errorf("failed to write inline prefix: %w", err)
	}

	// Write the actual data
	if _, err := w.writer.Write(data); err != nil {
		return fmt.Errorf("failed to write inline data: %w", err)
	}

	return nil
}

// WriteExternal writes a reference to an external file descriptor.
func (w *SplitFDStreamWriter) WriteExternal(fdIndex int) error {
	if fdIndex < 0 {
		return fmt.Errorf("file descriptor index must be non-negative, got %d", fdIndex)
	}

	// Write non-negative prefix referencing external fd
	prefix := int64(fdIndex)
	if err := binary.Write(w.writer, binary.LittleEndian, prefix); err != nil {
		return fmt.Errorf("failed to write external fd reference: %w", err)
	}

	return nil
}

// splitFDStreamReader reads data from the split FD stream format.
type splitFDStreamReader struct {
	reader     io.Reader
	fds        []*os.File               // static FD list (nil when using fdReceiver)
	fdReceiver func() (*os.File, error) // lazy FD receiver via SCM_RIGHTS (nil when using fds)
}

// newReader creates a new splitFDStreamReader with the provided file descriptors.
func newReader(r io.Reader, fds []*os.File) *splitFDStreamReader {
	return &splitFDStreamReader{
		reader: r,
		fds:    fds,
	}
}

// newReaderWithFDReceiver creates a splitFDStreamReader that receives FDs
// lazily via the provided callback, one at a time, as external chunks are
// encountered. This avoids holding all FDs open simultaneously.
func newReaderWithFDReceiver(r io.Reader, recv func() (*os.File, error)) *splitFDStreamReader {
	return &splitFDStreamReader{
		reader:     r,
		fdReceiver: recv,
	}
}

// chunkType represents the type of chunk in the split FD stream.
type chunkType int

const (
	chunkTypeInline chunkType = iota
	chunkTypeExternal
)

// chunk represents a single chunk in the split FD stream.
type chunk struct {
	typ     chunkType
	data    []byte   // For inline chunks
	fdIndex int      // For external chunks
	file    *os.File // For external chunks (resolved from fdIndex)
}

// nextChunk reads and returns the next chunk from the stream.
// Returns (chunk, nil) on success, (nil, io.EOF) at end of stream,
// or (nil, error) on failure.
func (r *splitFDStreamReader) nextChunk() (*chunk, error) {
	var prefix int64
	if err := binary.Read(r.reader, binary.LittleEndian, &prefix); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("failed to read chunk prefix: %w", err)
	}

	if prefix < 0 {
		// Inline data chunk
		dataLen := int(-prefix)
		data := make([]byte, dataLen)
		if _, err := io.ReadFull(r.reader, data); err != nil {
			return nil, fmt.Errorf("failed to read inline data of length %d: %w", dataLen, err)
		}

		return &chunk{
			typ:  chunkTypeInline,
			data: data,
		}, nil
	} else {
		// External file descriptor reference
		fdIndex := int(prefix)

		var file *os.File
		if r.fdReceiver != nil {
			// Receive FD lazily via SCM_RIGHTS
			f, err := r.fdReceiver()
			if err != nil {
				return nil, fmt.Errorf("failed to receive FD for index %d: %w", fdIndex, err)
			}
			file = f
		} else {
			if fdIndex >= len(r.fds) {
				return nil, fmt.Errorf("file descriptor index %d out of range (have %d fds)", fdIndex, len(r.fds))
			}
			file = r.fds[fdIndex]
		}

		return &chunk{
			typ:     chunkTypeExternal,
			fdIndex: fdIndex,
			file:    file,
		}, nil
	}
}
