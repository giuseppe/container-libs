package minimal

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/vbatts/tar-split/archive/tar"
)

func newTestZstdWriter(dest io.Writer) (ZstdWriter, error) {
	return zstd.NewWriter(dest, zstd.WithEncoderLevel(zstd.SpeedFastest))
}

func TestWriteZstdChunkedManifestV2NilTarSplit(t *testing.T) {
	var buf bytes.Buffer
	metadata := make(map[string]string)
	entries := []FileMetadata{
		{Type: TypeReg, Name: "file.txt", Mode: 0o644, Size: 5},
	}

	err := WriteZstdChunkedManifest(&buf, metadata, 0, nil, entries, newTestZstdWriter)
	if err != nil {
		t.Fatalf("WriteZstdChunkedManifest failed: %v", err)
	}

	// Should have manifest info but NO tarsplit info
	if _, ok := metadata[ManifestInfoKey]; !ok {
		t.Error("ManifestInfoKey should be present")
	}
	if _, ok := metadata[TarSplitInfoKey]; ok {
		t.Error("TarSplitInfoKey should NOT be present for v2")
	}
	if _, ok := metadata[ManifestChecksumKey]; !ok {
		t.Error("ManifestChecksumKey should be present")
	}
}

func TestWriteZstdChunkedManifestV1WithTarSplit(t *testing.T) {
	var buf bytes.Buffer
	metadata := make(map[string]string)
	entries := []FileMetadata{
		{Type: TypeReg, Name: "file.txt", Mode: 0o644, Size: 5},
	}

	ts := &TarSplitData{
		Data:             []byte("fake-tarsplit-data"),
		Digest:           "sha256:abcd1234",
		UncompressedSize: 100,
	}

	err := WriteZstdChunkedManifest(&buf, metadata, 0, ts, entries, newTestZstdWriter)
	if err != nil {
		t.Fatalf("WriteZstdChunkedManifest failed: %v", err)
	}

	// Should have both manifest and tarsplit info
	if _, ok := metadata[ManifestInfoKey]; !ok {
		t.Error("ManifestInfoKey should be present")
	}
	if _, ok := metadata[TarSplitInfoKey]; !ok {
		t.Error("TarSplitInfoKey should be present for v1")
	}
}

func TestFileMetadataToCanonicalTarHeader(t *testing.T) {
	modTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		fm   FileMetadata
	}{
		{
			name: "regular file",
			fm: FileMetadata{
				Type:    TypeReg,
				Name:    "usr/bin/test",
				Mode:    0o755,
				Size:    42,
				UID:     1000,
				GID:     1000,
				ModTime: &modTime,
			},
		},
		{
			name: "directory",
			fm: FileMetadata{
				Type:    TypeDir,
				Name:    "usr/lib/",
				Mode:    0o755,
				ModTime: &modTime,
			},
		},
		{
			name: "hardlink zeroes size",
			fm: FileMetadata{
				Type:     TypeLink,
				Name:     "usr/bin/link",
				Linkname: "usr/bin/target",
				Mode:     0o755,
				Size:     100, // Should be zeroed
			},
		},
		{
			name: "symlink",
			fm: FileMetadata{
				Type:     TypeSymlink,
				Name:     "usr/lib/libfoo.so",
				Linkname: "libfoo.so.1",
				Mode:     0o777,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr, err := FileMetadataToCanonicalTarHeader(&tt.fm)
			if err != nil {
				t.Fatalf("FileMetadataToCanonicalTarHeader failed: %v", err)
			}

			if hdr.Format != tar.FormatPAX {
				t.Errorf("Format: got %v, want FormatPAX", hdr.Format)
			}
			if hdr.PAXRecords["CONTAINERS.canonical"] != "1" {
				t.Error("missing CONTAINERS.canonical=1 PAX record")
			}
			if hdr.AccessTime.IsZero() != true {
				t.Error("AccessTime should be zero")
			}
			if hdr.ChangeTime.IsZero() != true {
				t.Error("ChangeTime should be zero")
			}

			if tt.fm.Type == TypeLink && hdr.Size != 0 {
				t.Errorf("hardlink Size: got %d, want 0", hdr.Size)
			}
		})
	}
}

func TestWriteCanonicalTar(t *testing.T) {
	modTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	entries := []FileMetadata{
		{Type: TypeDir, Name: "usr/", Mode: 0o755, ModTime: &modTime},
		{Type: TypeReg, Name: "usr/file.txt", Mode: 0o644, Size: 5, ModTime: &modTime},
		{Type: TypeChunk, Name: "usr/file.txt", ChunkOffset: 0, ChunkSize: 5}, // should be skipped
		{Type: TypeSymlink, Name: "usr/link", Linkname: "file.txt", Mode: 0o777, ModTime: &modTime},
	}

	var buf bytes.Buffer
	err := WriteCanonicalTar(&buf, entries, func(name string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("hello"))), nil
	})
	if err != nil {
		t.Fatalf("WriteCanonicalTar failed: %v", err)
	}

	// Read back and verify
	tr := tar.NewReader(&buf)

	// Entry 1: directory
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("reading dir entry: %v", err)
	}
	if hdr.Name != "usr/" {
		t.Errorf("expected 'usr/', got %q", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeDir {
		t.Errorf("expected TypeDir, got %d", hdr.Typeflag)
	}

	// Entry 2: file
	hdr, err = tr.Next()
	if err != nil {
		t.Fatalf("reading file entry: %v", err)
	}
	if hdr.Name != "usr/file.txt" {
		t.Errorf("expected 'usr/file.txt', got %q", hdr.Name)
	}
	content, _ := io.ReadAll(tr)
	if string(content) != "hello" {
		t.Errorf("expected 'hello', got %q", string(content))
	}

	// Entry 3: symlink (chunk entry was skipped)
	hdr, err = tr.Next()
	if err != nil {
		t.Fatalf("reading symlink entry: %v", err)
	}
	if hdr.Name != "usr/link" {
		t.Errorf("expected 'usr/link', got %q", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeSymlink {
		t.Errorf("expected TypeSymlink, got %d", hdr.Typeflag)
	}

	// Should be EOF
	_, err = tr.Next()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestWriteCanonicalTarDeterminism(t *testing.T) {
	modTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	entries := []FileMetadata{
		{Type: TypeDir, Name: "usr/", Mode: 0o755, ModTime: &modTime},
		{Type: TypeReg, Name: "usr/file.txt", Mode: 0o644, Size: 5, ModTime: &modTime},
	}

	writeTar := func() []byte {
		var buf bytes.Buffer
		err := WriteCanonicalTar(&buf, entries, func(name string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte("hello"))), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	tar1 := writeTar()
	tar2 := writeTar()

	if !bytes.Equal(tar1, tar2) {
		t.Error("WriteCanonicalTar output is not deterministic")
	}
}
