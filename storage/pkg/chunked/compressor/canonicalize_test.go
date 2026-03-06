package compressor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vbatts/tar-split/archive/tar"
	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/chunked/internal/minimal"
)

// TestArchiveFilterMatchesMinimalWriteCanonicalTar is the most important
// cross-package test. It verifies that:
//   - archive.NewCanonicalTarFilter (standard archive/tar, used in Diff())
//   - minimal.WriteCanonicalTar (vbatts/tar-split/archive/tar, used in ApplyDiff)
//
// produce byte-identical output. If this fails, push-after-partial-pull
// will get a digest mismatch because the DiffID is computed by one path
// and verified by the other.
func TestArchiveFilterMatchesMinimalWriteCanonicalTar(t *testing.T) {
	now := time.Date(2024, 6, 15, 10, 30, 0, 123456789, time.UTC)
	content1 := []byte("hello world content\n")
	content2 := []byte("another file\n")

	// Build a "filesystem-style" tar (what Diff() produces)
	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/share/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/share/doc.txt",
				Mode:     0o644,
				Size:     int64(len(content1)),
				ModTime:  now,
				PAXRecords: map[string]string{
					archive.PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00",
				},
			},
			content: content1,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/share/readme",
				Mode:     0o644,
				Size:     int64(len(content2)),
				ModTime:  now,
			},
			content: content2,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "usr/share/link",
				Linkname: "doc.txt",
				Mode:     0o777,
				ModTime:  now,
			},
		},
	}
	inputTar := buildTar(t, entries)

	// Path 1: archive.NewCanonicalTarFilter (what Diff() uses)
	src := io.NopCloser(bytes.NewReader(inputTar))
	filtered := archive.NewCanonicalTarFilter(src)
	filteredBytes, err := io.ReadAll(filtered)
	require.NoError(t, err)

	// Path 2: Feed through compressor to get TOC, then use WriteCanonicalTar
	var compressorOutput bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&compressorOutput, annotations)
	require.NoError(t, err)
	_, err = wc.Write(inputTar)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	toc := extractTOCFromNoCompressionOutput(t, compressorOutput.Bytes(), annotations)

	// Build file contents map
	fileContents := make(map[string][]byte)
	for _, e := range entries {
		if len(e.content) > 0 {
			hdrCopy := *e.header
			minimal.CanonicalizeTarHeader(&hdrCopy)
			fileContents[hdrCopy.Name] = e.content
		}
	}

	fg := &mapFileGetter{files: fileContents}
	var reconstructed bytes.Buffer
	require.NoError(t, minimal.WriteCanonicalTar(toc, fg, &reconstructed))

	// Compare all three paths
	filteredDigest := digest.Canonical.FromBytes(filteredBytes)
	reconstructedDigest := digest.Canonical.FromBytes(reconstructed.Bytes())

	assert.Equal(t, filteredDigest, reconstructedDigest,
		"archive.NewCanonicalTarFilter digest (%s) != minimal.WriteCanonicalTar digest (%s)",
		filteredDigest, reconstructedDigest)
	assert.Equal(t, filteredBytes, reconstructed.Bytes(),
		"archive.NewCanonicalTarFilter output != minimal.WriteCanonicalTar output (len: %d vs %d)",
		len(filteredBytes), len(reconstructed.Bytes()))
}

// testTarEntry describes a tar entry for test construction.
type testTarEntry struct {
	header  *tar.Header
	content []byte
}

// buildTar creates an in-memory tar archive from the given entries.
func buildTar(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(e.header))
		if len(e.content) > 0 {
			_, err := tw.Write(e.content)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// mapFileGetter implements minimal.FileGetter using an in-memory map.
type mapFileGetter struct {
	files map[string][]byte
}

func (m *mapFileGetter) Get(filename string) (io.ReadCloser, error) {
	data, ok := m.files[filename]
	if !ok {
		return nil, fmt.Errorf("file not found: %s: %w", filename, os.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// extractCanonicalTarFromNoCompressionOutput extracts the uncompressed tar
// stream from the output of writeZstdChunkedStream with NoCompression.
// With NoCompression, the output is: [canonical tar bytes][manifest frame][footer frame].
// The manifest offset from annotations tells us where the tar stream ends.
func extractCanonicalTarFromNoCompressionOutput(t *testing.T, output []byte, annotations map[string]string) []byte {
	t.Helper()

	// Parse the manifest position annotation to get the offset
	info := annotations[minimal.ManifestInfoKey]
	require.NotEmpty(t, info, "ManifestInfoKey annotation missing")

	var offset, length, uncompressedLength, manifestType uint64
	_, err := fmt.Sscanf(info, "%d:%d:%d:%d", &offset, &length, &uncompressedLength, &manifestType)
	require.NoError(t, err)

	// The skippable frame header is 8 bytes before the manifest offset.
	// The canonical tar stream is everything before that.
	tarEnd := offset - 8
	require.LessOrEqual(t, tarEnd, uint64(len(output)), "manifest offset beyond output")

	return output[:tarEnd]
}

// extractTOCFromNoCompressionOutput parses the TOC from the NoCompression output.
func extractTOCFromNoCompressionOutput(t *testing.T, output []byte, annotations map[string]string) *minimal.TOC {
	t.Helper()

	info := annotations[minimal.ManifestInfoKey]
	var offset, length, uncompressedLength, manifestType uint64
	_, err := fmt.Sscanf(info, "%d:%d:%d:%d", &offset, &length, &uncompressedLength, &manifestType)
	require.NoError(t, err)

	// With NoCompression, the "compressed" manifest is just raw JSON.
	// Skip the 8-byte skippable frame header to get to the manifest data.
	manifestData := output[offset : offset+length]

	var toc minimal.TOC
	require.NoError(t, json.Unmarshal(manifestData, &toc))
	return &toc
}

// TestCompressorWriteCanonicalTarRoundTrip is the end-to-end integration test.
// It verifies that:
//  1. Feed a tar through writeZstdChunkedStream (NoCompression) → get canonical tar bytes + TOC
//  2. Use WriteCanonicalTar with the TOC → get reconstructed tar bytes
//  3. The two canonical tar streams are byte-identical
//
// This is the exact invariant needed for push-after-partial-pull:
// - Build creates the zstd:chunked blob (step 1)
// - Pull stores the TOC
// - Push needs to reproduce the same uncompressed tar (step 2)
func TestCompressorWriteCanonicalTarRoundTrip(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC)

	fileContent := bytes.Repeat([]byte("hello world\n"), 100) // 1200 bytes
	smallContent := []byte("small")

	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/bin/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/bin/hello",
				Mode:     0o755,
				Size:     int64(len(fileContent)),
				ModTime:  now,
			},
			content: fileContent,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "usr/bin/hi",
				Linkname: "hello",
				Mode:     0o777,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "etc/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "etc/config",
				Mode:     0o644,
				Size:     int64(len(smallContent)),
				ModTime:  now,
			},
			content: smallContent,
		},
	}

	tarData := buildTar(t, entries)

	// Step 1: Feed through writeZstdChunkedStream with NoCompression
	var compressedOutput bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&compressedOutput, annotations)
	require.NoError(t, err)

	_, err = wc.Write(tarData)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	output := compressedOutput.Bytes()

	// Extract canonical tar and TOC
	canonicalTarBytes := extractCanonicalTarFromNoCompressionOutput(t, output, annotations)
	toc := extractTOCFromNoCompressionOutput(t, output, annotations)

	require.Equal(t, 2, toc.Version, "expected v2 TOC (no tarsplit)")

	// Build file content map using canonical names from the TOC
	fileContents := make(map[string][]byte)
	for _, e := range toc.Entries {
		if e.Type == minimal.TypeChunk {
			continue
		}
		// Find content from our original entries
		for _, orig := range entries {
			if len(orig.content) > 0 {
				// Canonicalize the original name to match the TOC name
				origHdr := *orig.header
				minimal.CanonicalizeTarHeader(&origHdr)
				if origHdr.Name == e.Name {
					fileContents[e.Name] = orig.content
					break
				}
			}
		}
	}

	// Step 2: Reconstruct canonical tar from TOC using WriteCanonicalTar
	fg := &mapFileGetter{files: fileContents}
	var reconstructed bytes.Buffer
	err = minimal.WriteCanonicalTar(toc, fg, &reconstructed)
	require.NoError(t, err)

	// Step 3: Compare
	expectedDigest := digest.Canonical.FromBytes(canonicalTarBytes)
	actualDigest := digest.Canonical.FromBytes(reconstructed.Bytes())

	assert.Equal(t, expectedDigest, actualDigest,
		"Digest mismatch: compressor produced %s, WriteCanonicalTar produced %s",
		expectedDigest, actualDigest)

	if !bytes.Equal(canonicalTarBytes, reconstructed.Bytes()) {
		t.Errorf("Canonical tar bytes differ (compressor=%d bytes, WriteCanonicalTar=%d bytes)",
			len(canonicalTarBytes), len(reconstructed.Bytes()))
		// Find first differing byte for debugging
		minLen := len(canonicalTarBytes)
		if len(reconstructed.Bytes()) < minLen {
			minLen = len(reconstructed.Bytes())
		}
		for i := 0; i < minLen; i++ {
			if canonicalTarBytes[i] != reconstructed.Bytes()[i] {
				t.Errorf("First difference at byte %d: compressor=0x%02x, reconstructed=0x%02x",
					i, canonicalTarBytes[i], reconstructed.Bytes()[i])
				break
			}
		}
	}
}

// TestCompressorVariantInputsSameOutput verifies that feeding two different
// tar representations of the same content through the compressor produces
// identical canonical output. This simulates the build-time vs push-time
// scenario where tar headers differ in formatting.
func TestCompressorVariantInputsSameOutput(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	content := []byte("file content here\n")

	// "Archive-style" tar: ./ prefix, file type bits in Mode, Uname set, extra PAX
	archiveTar := buildTar(t, []testTarEntry{
		{
			header: &tar.Header{
				Typeflag:   tar.TypeDir,
				Name:       "./usr/",
				Mode:       0o40755,
				Uname:      "root",
				Gname:      "root",
				ModTime:    now,
				AccessTime: now,
				ChangeTime: now,
			},
		},
		{
			header: &tar.Header{
				Typeflag:   tar.TypeDir,
				Name:       "./usr/share/",
				Mode:       0o40755,
				Uname:      "root",
				Gname:      "root",
				ModTime:    now,
				AccessTime: now,
				ChangeTime: now,
			},
		},
		{
			header: &tar.Header{
				Typeflag:   tar.TypeReg,
				Name:       "./usr/share/doc.txt",
				Mode:       0o100644,
				Size:       int64(len(content)),
				Uname:      "root",
				Gname:      "root",
				ModTime:    now,
				AccessTime: now,
				ChangeTime: now,
				PAXRecords: map[string]string{
					"mtime":                                        "1717243200.0",
					archive.PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02",
				},
			},
			content: content,
		},
	})

	// "Filesystem-style" tar: no ./ prefix, no file type bits, no Uname
	fsTar := buildTar(t, []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/share",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/share/doc.txt",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
				PAXRecords: map[string]string{
					archive.PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02",
				},
			},
			content: content,
		},
	})

	// Feed both through the compressor
	var archiveOutput, fsOutput bytes.Buffer
	archiveAnnotations := make(map[string]string)
	fsAnnotations := make(map[string]string)

	wc1, err := NoCompression(&archiveOutput, archiveAnnotations)
	require.NoError(t, err)
	_, err = wc1.Write(archiveTar)
	require.NoError(t, err)
	require.NoError(t, wc1.Close())

	wc2, err := NoCompression(&fsOutput, fsAnnotations)
	require.NoError(t, err)
	_, err = wc2.Write(fsTar)
	require.NoError(t, err)
	require.NoError(t, wc2.Close())

	// Extract canonical tar streams
	archiveCanonical := extractCanonicalTarFromNoCompressionOutput(t, archiveOutput.Bytes(), archiveAnnotations)
	fsCanonical := extractCanonicalTarFromNoCompressionOutput(t, fsOutput.Bytes(), fsAnnotations)

	archiveDigest := digest.Canonical.FromBytes(archiveCanonical)
	fsDigest := digest.Canonical.FromBytes(fsCanonical)

	assert.Equal(t, archiveDigest, fsDigest,
		"Archive-style and filesystem-style inputs produce different canonical digests: archive=%s, fs=%s",
		archiveDigest, fsDigest)

	assert.Equal(t, archiveCanonical, fsCanonical,
		"Archive-style and filesystem-style inputs produce different canonical tar bytes")
}

// TestCompressorRoundTripWithXattrs tests that xattrs survive the full
// compressor → TOC → WriteCanonicalTar round-trip.
func TestCompressorRoundTripWithXattrs(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("binary data")

	tarData := buildTar(t, []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/sbin/ping",
				Mode:     0o755,
				Size:     int64(len(content)),
				ModTime:  now,
				PAXRecords: map[string]string{
					archive.PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00",
				},
			},
			content: content,
		},
	})

	var output bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&output, annotations)
	require.NoError(t, err)
	_, err = wc.Write(tarData)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	canonicalTarBytes := extractCanonicalTarFromNoCompressionOutput(t, output.Bytes(), annotations)
	toc := extractTOCFromNoCompressionOutput(t, output.Bytes(), annotations)

	fg := &mapFileGetter{files: map[string][]byte{"usr/sbin/ping": content}}
	var reconstructed bytes.Buffer
	require.NoError(t, minimal.WriteCanonicalTar(toc, fg, &reconstructed))

	assert.Equal(t, canonicalTarBytes, reconstructed.Bytes(),
		"Xattr round-trip through compressor produced different output")
}

// TestCompressorRoundTripWithHardlinks tests hardlink handling through
// the full compressor pipeline.
func TestCompressorRoundTripWithHardlinks(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("shared content")

	tarData := buildTar(t, []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/lib/libfoo.so.1.0",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
			},
			content: content,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeLink,
				Name:     "usr/lib/libfoo.so.1",
				Linkname: "usr/lib/libfoo.so.1.0",
				Mode:     0o644,
				Size:     int64(len(content)), // non-zero, should be zeroed
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeLink,
				Name:     "usr/lib/libfoo.so",
				Linkname: "usr/lib/libfoo.so.1.0",
				Mode:     0o644,
				ModTime:  now,
			},
		},
	})

	var output bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&output, annotations)
	require.NoError(t, err)
	_, err = wc.Write(tarData)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	canonicalTarBytes := extractCanonicalTarFromNoCompressionOutput(t, output.Bytes(), annotations)
	toc := extractTOCFromNoCompressionOutput(t, output.Bytes(), annotations)

	fg := &mapFileGetter{files: map[string][]byte{"usr/lib/libfoo.so.1.0": content}}
	var reconstructed bytes.Buffer
	require.NoError(t, minimal.WriteCanonicalTar(toc, fg, &reconstructed))

	assert.Equal(t, canonicalTarBytes, reconstructed.Bytes(),
		"Hardlink round-trip through compressor produced different output")
}

// TestCompressorReportsCanonicalDigest verifies that the compressor writes
// the canonical uncompressed digest into its metadata annotations, and that
// this digest matches what WriteCanonicalTar produces.
func TestCompressorReportsCanonicalDigest(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("test content for digest verification\n")

	tarData := buildTar(t, []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/file.txt",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
			},
			content: content,
		},
	})

	var output bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&output, annotations)
	require.NoError(t, err)
	_, err = wc.Write(tarData)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	// Verify the annotation is present
	canonicalDigestStr, ok := annotations[minimal.UncompressedDigestKey]
	require.True(t, ok, "compressor should set %s annotation", minimal.UncompressedDigestKey)

	canonicalDigest, err := digest.Parse(canonicalDigestStr)
	require.NoError(t, err)

	// Verify it matches the actual canonical tar content
	canonicalTarBytes := extractCanonicalTarFromNoCompressionOutput(t, output.Bytes(), annotations)
	actualDigest := digest.Canonical.FromBytes(canonicalTarBytes)
	assert.Equal(t, actualDigest, canonicalDigest,
		"reported canonical digest %s != actual canonical tar digest %s", canonicalDigest, actualDigest)

	// Verify it matches what WriteCanonicalTar produces
	toc := extractTOCFromNoCompressionOutput(t, output.Bytes(), annotations)
	hdrCopy := tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "usr/file.txt",
		Mode:     0o644,
		Size:     int64(len(content)),
		ModTime:  now,
	}
	minimal.CanonicalizeTarHeader(&hdrCopy)
	fg := &mapFileGetter{files: map[string][]byte{hdrCopy.Name: content}}
	var reconstructed bytes.Buffer
	require.NoError(t, minimal.WriteCanonicalTar(toc, fg, &reconstructed))
	reconstructedDigest := digest.Canonical.FromBytes(reconstructed.Bytes())
	assert.Equal(t, canonicalDigest, reconstructedDigest,
		"reported canonical digest %s != WriteCanonicalTar digest %s", canonicalDigest, reconstructedDigest)
}

// TestCompressorRoundTripLargeFile verifies that the canonical digest is
// correct for files large enough to be split into multiple chunks by the
// rolling checksum. This catches a bug where payloadDest dropped the
// canonicalDigester after the first chunk split.
func TestCompressorRoundTripLargeFile(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	// Create a file large enough to trigger multiple chunk splits.
	// RollsumBits=16 means chunks are ~64KB on average, so 1MB of
	// pseudo-random data should produce several chunks.
	largeContent := make([]byte, 1024*1024)
	// Use a simple LCG to generate pseudo-random bytes that will
	// trigger the rolling checksum splits.
	v := uint32(12345)
	for i := range largeContent {
		v = v*1103515245 + 12345
		largeContent[i] = byte(v >> 16)
	}

	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/largefile",
				Mode:     0o644,
				Size:     int64(len(largeContent)),
				ModTime:  now,
			},
			content: largeContent,
		},
	}

	tarData := buildTar(t, entries)

	// Feed through compressor
	var compressedOutput bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&compressedOutput, annotations)
	require.NoError(t, err)
	_, err = wc.Write(tarData)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	output := compressedOutput.Bytes()

	// Extract canonical tar and TOC
	canonicalTarBytes := extractCanonicalTarFromNoCompressionOutput(t, output, annotations)
	toc := extractTOCFromNoCompressionOutput(t, output, annotations)

	require.Equal(t, 2, toc.Version, "expected v2 TOC")

	// Verify the TOC has chunks (confirms the file was split)
	chunkCount := 0
	for _, e := range toc.Entries {
		if e.Type == minimal.TypeChunk {
			chunkCount++
		}
	}
	require.Greater(t, chunkCount, 0, "expected file to be split into chunks")

	// Build file content map
	fileContents := make(map[string][]byte)
	for _, e := range toc.Entries {
		if e.Type == minimal.TypeChunk {
			continue
		}
		for _, orig := range entries {
			if len(orig.content) > 0 {
				origHdr := *orig.header
				minimal.CanonicalizeTarHeader(&origHdr)
				if origHdr.Name == e.Name {
					fileContents[e.Name] = orig.content
					break
				}
			}
		}
	}

	// Reconstruct from TOC
	fg := &mapFileGetter{files: fileContents}
	var reconstructed bytes.Buffer
	require.NoError(t, minimal.WriteCanonicalTar(toc, fg, &reconstructed))

	// Compare compressor output vs WriteCanonicalTar reconstruction
	expectedDigest := digest.Canonical.FromBytes(canonicalTarBytes)
	actualDigest := digest.Canonical.FromBytes(reconstructed.Bytes())
	assert.Equal(t, expectedDigest, actualDigest,
		"Digest mismatch for large file: compressor=%s, WriteCanonicalTar=%s",
		expectedDigest, actualDigest)

	// Also verify the annotation matches
	annotatedDigest, ok := annotations[minimal.UncompressedDigestKey]
	require.True(t, ok, "UncompressedDigestKey annotation missing")
	assert.Equal(t, expectedDigest.String(), annotatedDigest,
		"Annotation digest %s != actual canonical tar digest %s",
		annotatedDigest, expectedDigest)
}

// TestArchiveWriteCanonicalTarMatchesMinimal is the cross-package test that
// verifies archive.WriteCanonicalTar (standard archive/tar, used in Diff()
// during push) produces byte-identical output to minimal.WriteCanonicalTar
// (vbatts/tar-split/archive/tar, used in ApplyDiff during pull).
// If this fails, push-after-partial-pull will get a digest mismatch.
func TestArchiveWriteCanonicalTarMatchesMinimal(t *testing.T) {
	now := time.Date(2024, 6, 15, 10, 30, 0, 123456789, time.UTC)
	content1 := []byte("hello world content\n")
	content2 := []byte("another file\n")

	// Build a tar, feed through compressor to get TOC
	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/share/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/share/doc.txt",
				Mode:     0o644,
				Size:     int64(len(content1)),
				ModTime:  now,
				PAXRecords: map[string]string{
					archive.PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00",
				},
			},
			content: content1,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/share/readme",
				Mode:     0o644,
				Size:     int64(len(content2)),
				ModTime:  now,
			},
			content: content2,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "usr/share/link",
				Linkname: "doc.txt",
				Mode:     0o777,
				ModTime:  now,
			},
		},
	}
	inputTar := buildTar(t, entries)

	// Feed through compressor to get TOC (the source of truth)
	var compressorOutput bytes.Buffer
	annotations := make(map[string]string)
	wc, err := NoCompression(&compressorOutput, annotations)
	require.NoError(t, err)
	_, err = wc.Write(inputTar)
	require.NoError(t, err)
	require.NoError(t, wc.Close())

	toc := extractTOCFromNoCompressionOutput(t, compressorOutput.Bytes(), annotations)
	tocJSON, err := json.Marshal(toc)
	require.NoError(t, err)

	// Build file contents map using canonical names from the TOC
	fileContents := make(map[string][]byte)
	for _, e := range entries {
		if len(e.content) > 0 {
			hdrCopy := *e.header
			minimal.CanonicalizeTarHeader(&hdrCopy)
			fileContents[hdrCopy.Name] = e.content
		}
	}

	// Path 1: minimal.WriteCanonicalTar (used during pull in ApplyDiff)
	minimalFG := &mapFileGetter{files: fileContents}
	var minimalOutput bytes.Buffer
	require.NoError(t, minimal.WriteCanonicalTar(toc, minimalFG, &minimalOutput))

	// Path 2: archive.WriteCanonicalTar (used during push in Diff)
	archiveFG := &mapFileGetter{files: fileContents}
	var archiveOutput bytes.Buffer
	require.NoError(t, archive.WriteCanonicalTar(bytes.NewReader(tocJSON), archiveFG, &archiveOutput))

	// Compare
	minimalDigest := digest.Canonical.FromBytes(minimalOutput.Bytes())
	archiveDigest := digest.Canonical.FromBytes(archiveOutput.Bytes())

	assert.Equal(t, minimalDigest, archiveDigest,
		"minimal.WriteCanonicalTar digest (%s) != archive.WriteCanonicalTar digest (%s)",
		minimalDigest, archiveDigest)
	assert.Equal(t, minimalOutput.Bytes(), archiveOutput.Bytes(),
		"minimal.WriteCanonicalTar output != archive.WriteCanonicalTar output (len: %d vs %d)",
		len(minimalOutput.Bytes()), len(archiveOutput.Bytes()))
}

