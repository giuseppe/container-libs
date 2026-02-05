package splitfdstream

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSplitFDStreamWriter_WriteInline(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		wantErr  bool
		expected []byte
	}{
		{
			name:     "simple inline data",
			data:     []byte("hello"),
			wantErr:  false,
			expected: append([]byte{251, 255, 255, 255, 255, 255, 255, 255}, []byte("hello")...), // -5 in little-endian + "hello"
		},
		{
			name:    "empty data",
			data:    []byte{},
			wantErr: true,
		},
		{
			name:     "single byte",
			data:     []byte{42},
			wantErr:  false,
			expected: append([]byte{255, 255, 255, 255, 255, 255, 255, 255}, []byte{42}...), // -1 in little-endian + byte{42}
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)

			err := w.WriteInline(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("WriteInline() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				got := buf.Bytes()
				if !bytes.Equal(got, tt.expected) {
					t.Errorf("WriteInline() wrote %v, want %v", got, tt.expected)
				}
			}
		})
	}
}

func TestSplitFDStreamWriter_WriteExternal(t *testing.T) {
	tests := []struct {
		name     string
		fdIndex  int
		wantErr  bool
		expected []byte
	}{
		{
			name:     "fd index 0",
			fdIndex:  0,
			wantErr:  false,
			expected: []byte{0, 0, 0, 0, 0, 0, 0, 0}, // 0 in little-endian
		},
		{
			name:     "fd index 1",
			fdIndex:  1,
			wantErr:  false,
			expected: []byte{1, 0, 0, 0, 0, 0, 0, 0}, // 1 in little-endian
		},
		{
			name:     "fd index 255",
			fdIndex:  255,
			wantErr:  false,
			expected: []byte{255, 0, 0, 0, 0, 0, 0, 0}, // 255 in little-endian
		},
		{
			name:    "negative fd index",
			fdIndex: -1,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)

			err := w.WriteExternal(tt.fdIndex)
			if (err != nil) != tt.wantErr {
				t.Errorf("WriteExternal() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				got := buf.Bytes()
				if !bytes.Equal(got, tt.expected) {
					t.Errorf("WriteExternal() wrote %v, want %v", got, tt.expected)
				}
			}
		})
	}
}

func TestSplitFDStreamReader_nextChunk(t *testing.T) {
	// Create a temporary file for external reference tests
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")
	testData := []byte("test file content")
	if err := os.WriteFile(tmpFile, testData, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	file, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("Failed to open test file: %v", err)
	}
	defer file.Close()

	tests := []struct {
		name     string
		input    []byte
		fds      []*os.File
		wantType chunkType
		wantData []byte
		wantFD   int
		wantErr  bool
	}{
		{
			name:     "inline chunk",
			input:    append([]byte{251, 255, 255, 255, 255, 255, 255, 255}, []byte("hello")...), // -5 + "hello"
			fds:      nil,
			wantType: chunkTypeInline,
			wantData: []byte("hello"),
			wantErr:  false,
		},
		{
			name:     "external chunk",
			input:    []byte{0, 0, 0, 0, 0, 0, 0, 0}, // fd index 0
			fds:      []*os.File{file},
			wantType: chunkTypeExternal,
			wantFD:   0,
			wantErr:  false,
		},
		{
			name:    "external chunk with invalid fd index",
			input:   []byte{1, 0, 0, 0, 0, 0, 0, 0}, // fd index 1 (out of range)
			fds:     []*os.File{file},
			wantErr: true,
		},
		{
			name:    "truncated prefix",
			input:   []byte{1, 2, 3}, // incomplete 8-byte prefix
			fds:     nil,
			wantErr: true,
		},
		{
			name:    "truncated inline data",
			input:   []byte{251, 255, 255, 255, 255, 255, 255, 255, 'h', 'e'}, // -5 prefix but only 2 bytes
			fds:     nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := bytes.NewReader(tt.input)
			r := newReader(buf, tt.fds)

			c, err := r.nextChunk()
			if (err != nil) != tt.wantErr {
				t.Errorf("nextChunk() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if c.typ != tt.wantType {
				t.Errorf("nextChunk() chunk type = %v, want %v", c.typ, tt.wantType)
			}

			switch tt.wantType {
			case chunkTypeInline:
				if !bytes.Equal(c.data, tt.wantData) {
					t.Errorf("nextChunk() chunk data = %v, want %v", c.data, tt.wantData)
				}
			case chunkTypeExternal:
				if c.fdIndex != tt.wantFD {
					t.Errorf("nextChunk() chunk FD index = %v, want %v", c.fdIndex, tt.wantFD)
				}
				if c.file != tt.fds[tt.wantFD] {
					t.Errorf("nextChunk() chunk file pointer incorrect")
				}
			}
		})
	}
}

func TestSplitFDStreamReader_EOF(t *testing.T) {
	buf := bytes.NewReader([]byte{})
	r := newReader(buf, nil)

	c, err := r.nextChunk()
	if err != io.EOF {
		t.Errorf("nextChunk() on empty stream should return io.EOF, got %v", err)
	}
	if c != nil {
		t.Errorf("nextChunk() on empty stream should return nil chunk, got %v", c)
	}
}

func TestSplitFDStreamRoundTrip(t *testing.T) {
	// Create test file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.txt")
	testFileData := []byte("external file content")
	if err := os.WriteFile(tmpFile, testFileData, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	file, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("Failed to open test file: %v", err)
	}
	defer file.Close()

	// Write splitfdstream
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// Write inline chunk
	if err := w.WriteInline([]byte("inline data")); err != nil {
		t.Fatalf("Failed to write inline chunk: %v", err)
	}

	// Write external reference
	if err := w.WriteExternal(0); err != nil {
		t.Fatalf("Failed to write external chunk: %v", err)
	}

	// Write another inline chunk
	if err := w.WriteInline([]byte("more inline")); err != nil {
		t.Fatalf("Failed to write second inline chunk: %v", err)
	}

	// Read back the splitfdstream
	r := newReader(bytes.NewReader(buf.Bytes()), []*os.File{file})

	// Read first chunk (inline)
	c1, err := r.nextChunk()
	if err != nil {
		t.Fatalf("Failed to read first chunk: %v", err)
	}
	if c1.typ != chunkTypeInline {
		t.Errorf("Expected first chunk to be inline, got %v", c1.typ)
	}
	if !bytes.Equal(c1.data, []byte("inline data")) {
		t.Errorf("First chunk data = %v, want %v", c1.data, []byte("inline data"))
	}

	// Read second chunk (external)
	c2, err := r.nextChunk()
	if err != nil {
		t.Fatalf("Failed to read second chunk: %v", err)
	}
	if c2.typ != chunkTypeExternal {
		t.Errorf("Expected second chunk to be external, got %v", c2.typ)
	}
	if c2.fdIndex != 0 {
		t.Errorf("Second chunk FD index = %v, want 0", c2.fdIndex)
	}

	// Read third chunk (inline)
	c3, err := r.nextChunk()
	if err != nil {
		t.Fatalf("Failed to read third chunk: %v", err)
	}
	if c3.typ != chunkTypeInline {
		t.Errorf("Expected third chunk to be inline, got %v", c3.typ)
	}
	if !bytes.Equal(c3.data, []byte("more inline")) {
		t.Errorf("Third chunk data = %v, want %v", c3.data, []byte("more inline"))
	}

	// Verify EOF
	_, err = r.nextChunk()
	if err != io.EOF {
		t.Errorf("Expected EOF after reading all chunks, got %v", err)
	}
}
