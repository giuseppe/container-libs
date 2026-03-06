package compressor

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/vbatts/tar-split/archive/tar"
	"go.podman.io/storage/pkg/chunked/internal/minimal"
)

func newTestZstdWriter(dest io.Writer) (minimal.ZstdWriter, error) {
	return zstd.NewWriter(dest, zstd.WithEncoderLevel(zstd.SpeedFastest))
}

func createTestTar(canonical bool) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	paxRecords := map[string]string{}
	if canonical {
		paxRecords["CONTAINERS.canonical"] = "1"
	}

	hdr := &tar.Header{
		Name:       "file.txt",
		Typeflag:   tar.TypeReg,
		Mode:       0o644,
		Size:       5,
		Format:     tar.FormatPAX,
		PAXRecords: paxRecords,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		panic(err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		panic(err)
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestCanonicalInputProducesV2(t *testing.T) {
	tarData := createTestTar(true)
	var output bytes.Buffer
	metadata := make(map[string]string)

	err := writeZstdChunkedStream(&output, metadata, bytes.NewReader(tarData), newTestZstdWriter)
	if err != nil {
		t.Fatalf("writeZstdChunkedStream failed: %v", err)
	}

	// v2: should NOT have TarSplitInfoKey in metadata
	if _, ok := metadata[minimal.TarSplitInfoKey]; ok {
		t.Error("canonical input should produce v2 TOC without tarsplit, but TarSplitInfoKey is present")
	}
}

func TestNonCanonicalInputProducesV1(t *testing.T) {
	tarData := createTestTar(false)
	var output bytes.Buffer
	metadata := make(map[string]string)

	err := writeZstdChunkedStream(&output, metadata, bytes.NewReader(tarData), newTestZstdWriter)
	if err != nil {
		t.Fatalf("writeZstdChunkedStream failed: %v", err)
	}

	// v1: should have TarSplitInfoKey in metadata
	if _, ok := metadata[minimal.TarSplitInfoKey]; !ok {
		t.Error("non-canonical input should produce v1 TOC with tarsplit, but TarSplitInfoKey is missing")
	}
}
