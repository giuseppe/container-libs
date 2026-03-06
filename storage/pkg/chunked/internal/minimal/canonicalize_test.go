package minimal

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vbatts/tar-split/archive/tar"
	"go.podman.io/storage/pkg/archive"
)

// TestCanonicalizeTarHeaderIdempotent verifies that applying
// CanonicalizeTarHeader twice yields the same result as once.
func TestCanonicalizeTarHeaderIdempotent(t *testing.T) {
	now := time.Now()
	hdr := &tar.Header{
		Typeflag:   tar.TypeReg,
		Name:       "./usr/bin/hello",
		Mode:       0o100755, // includes file type bits
		Size:       42,
		Uid:        1000,
		Gid:        1000,
		Uname:      "user",
		Gname:      "group",
		ModTime:    now,
		AccessTime: now,
		ChangeTime: now,
		PAXRecords: map[string]string{
			"mtime":                             "1234567890.123456789",
			"atime":                             "1234567890.123456789",
			archive.PaxSchilyXattr + "security.capability": "\x01\x00",
			"GOLANG.pkg.main":                   "1",
		},
	}

	CanonicalizeTarHeader(hdr)
	first, err := CanonicalHeaderBytes(hdr)
	require.NoError(t, err)

	CanonicalizeTarHeader(hdr)
	second, err := CanonicalHeaderBytes(hdr)
	require.NoError(t, err)

	assert.Equal(t, first, second, "CanonicalizeTarHeader is not idempotent")
}

// TestCanonicalizeNormalizesMode verifies that file type bits in Mode are
// stripped, so headers from tar.Reader (which preserves them) and
// tar.FileInfoHeader (which doesn't since Go 1.9) produce the same result.
func TestCanonicalizeNormalizesMode(t *testing.T) {
	// Simulate a header read from an archive (has file type bits)
	archiveHdr := &tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "usr/lib/",
		Mode:     0o40755, // directory type bits + permissions
	}

	// Simulate a header from FileInfoHeader (no file type bits)
	fsHdr := &tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "usr/lib/",
		Mode:     0o755, // permissions only
	}

	archiveBytes, err := CanonicalHeaderBytes(archiveHdr)
	require.NoError(t, err)

	fsBytes, err := CanonicalHeaderBytes(fsHdr)
	require.NoError(t, err)

	assert.Equal(t, archiveBytes, fsBytes,
		"Mode normalization failed: archive header and filesystem header produce different output")
}

// TestCanonicalizeNormalizesPath verifies that path variations are normalized.
func TestCanonicalizeNormalizesPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		typeflag byte
		expected string
	}{
		{"strip dot-slash prefix", "./usr/bin/hello", tar.TypeReg, "usr/bin/hello"},
		{"dir gets trailing slash", "usr/lib", tar.TypeDir, "usr/lib/"},
		{"dir with dot-slash", "./usr/lib/", tar.TypeDir, "usr/lib/"},
		{"clean double slash", "usr//bin///hello", tar.TypeReg, "usr/bin/hello"},
		{"regular file no change", "usr/bin/hello", tar.TypeReg, "usr/bin/hello"},
		{"symlink target not changed by isDir", "usr/lib", tar.TypeSymlink, "usr/lib"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdr := &tar.Header{
				Typeflag: tc.typeflag,
				Name:     tc.input,
				Mode:     0o755,
			}
			CanonicalizeTarHeader(hdr)
			assert.Equal(t, tc.expected, hdr.Name)
		})
	}
}

// TestCanonicalizeStripsTimestamps verifies that AccessTime and ChangeTime
// are zeroed but ModTime is preserved.
func TestCanonicalizeStripsTimestamps(t *testing.T) {
	now := time.Now()
	hdr := &tar.Header{
		Typeflag:   tar.TypeReg,
		Name:       "file.txt",
		Mode:       0o644,
		ModTime:    now,
		AccessTime: now,
		ChangeTime: now,
	}
	CanonicalizeTarHeader(hdr)
	assert.Equal(t, now, hdr.ModTime, "ModTime should be preserved")
	assert.True(t, hdr.AccessTime.IsZero(), "AccessTime should be zeroed")
	assert.True(t, hdr.ChangeTime.IsZero(), "ChangeTime should be zeroed")
}

// TestCanonicalizeStripsPAXRecords verifies that only SCHILY.xattr.* and
// the canonical marker PAX records survive canonicalization.
func TestCanonicalizeStripsPAXRecords(t *testing.T) {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "file.txt",
		Mode:     0o644,
		PAXRecords: map[string]string{
			"mtime":                                        "1234567890.1",
			"atime":                                        "1234567890.1",
			"ctime":                                        "1234567890.1",
			"SCHILY.fflags":                                "nosappnd",
			archive.PaxSchilyXattr + "security.capability": "\x01\x00",
			archive.PaxSchilyXattr + "user.test":           "val",
		},
	}
	CanonicalizeTarHeader(hdr)

	// Only SCHILY.xattr.* + our canonical marker should remain
	assert.Contains(t, hdr.PAXRecords, archive.PaxSchilyXattr+"security.capability")
	assert.Contains(t, hdr.PAXRecords, archive.PaxSchilyXattr+"user.test")
	assert.Contains(t, hdr.PAXRecords, canonicalPAXRecordKey)
	assert.NotContains(t, hdr.PAXRecords, "mtime")
	assert.NotContains(t, hdr.PAXRecords, "atime")
	assert.NotContains(t, hdr.PAXRecords, "ctime")
	assert.NotContains(t, hdr.PAXRecords, "SCHILY.fflags")
}

// TestCanonicalizeHardlink verifies hardlink size is zeroed.
func TestCanonicalizeHardlink(t *testing.T) {
	hdr := &tar.Header{
		Typeflag: tar.TypeLink,
		Name:     "link",
		Linkname: "target",
		Size:     100,
		Mode:     0o644,
	}
	CanonicalizeTarHeader(hdr)
	assert.Equal(t, int64(0), hdr.Size)
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

// canonicalTarFromEntries manually builds a canonical tar stream by reading
// a tar archive entry-by-entry and writing canonical headers + content.
// This mirrors what writeZstdChunkedStream does (minus compression/chunking).
func canonicalTarFromEntries(t *testing.T, tarData []byte) ([]byte, []FileMetadata) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(tarData))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var metadata []FileMetadata

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		CanonicalizeTarHeader(hdr)

		require.NoError(t, tw.WriteHeader(hdr))

		var contentBuf bytes.Buffer
		if hdr.Size > 0 {
			_, err := io.Copy(io.MultiWriter(tw, &contentBuf), tr)
			require.NoError(t, err)
		}

		fm, err := NewFileMetadata(hdr)
		require.NoError(t, err)

		if hdr.Size > 0 {
			fm.Digest = digest.Canonical.FromBytes(contentBuf.Bytes()).String()
		}

		metadata = append(metadata, fm)
	}

	require.NoError(t, tw.Close())
	return buf.Bytes(), metadata
}

// mapFileGetter implements FileGetter using an in-memory map.
type mapFileGetter struct {
	files map[string][]byte
}

func (m *mapFileGetter) Get(filename string) (io.ReadCloser, error) {
	data, ok := m.files[filename]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// TestWriteCanonicalTarMatchesCompressorOutput is the key invariant test.
// It verifies that WriteCanonicalTar (used during pull to compute
// UncompressedDigest) produces exactly the same bytes as the canonical
// tar stream written by the compressor (writeZstdChunkedStream).
//
// If this test fails, push after partial pull will get a digest mismatch.
func TestWriteCanonicalTarMatchesCompressorOutput(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC)

	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr/",
				Mode:     0o755,
				Uid:      0,
				Gid:      0,
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
				Size:     13,
				ModTime:  now,
			},
			content: []byte("hello, world\n"),
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
				Name:     "usr/lib/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/lib/libfoo.so",
				Mode:     0o644,
				Size:     4,
				ModTime:  now,
				PAXRecords: map[string]string{
					archive.PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00",
				},
			},
			content: []byte("ELF\n"),
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeLink,
				Name:     "usr/lib/libfoo.so.1",
				Linkname: "usr/lib/libfoo.so",
				Mode:     0o644,
				Size:     4, // non-zero size on hardlink - should be zeroed
				ModTime:  now,
			},
		},
	}

	tarData := buildTar(t, entries)
	canonicalBytes, metadata := canonicalTarFromEntries(t, tarData)

	// Build file content map for WriteCanonicalTar
	fileContents := make(map[string][]byte)
	for _, e := range entries {
		if len(e.content) > 0 {
			CanonicalizeTarHeader(e.header) // get canonical name
			fileContents[e.header.Name] = e.content
		}
	}

	toc := &TOC{
		Version: 2,
		Entries: metadata,
	}
	fg := &mapFileGetter{files: fileContents}

	var reconstructed bytes.Buffer
	err := WriteCanonicalTar(toc, fg, &reconstructed)
	require.NoError(t, err)

	assert.Equal(t, canonicalBytes, reconstructed.Bytes(),
		"WriteCanonicalTar output does not match canonical tar from compressor path")

	// Also verify digests match
	expectedDigest := digest.Canonical.FromBytes(canonicalBytes)
	actualDigest := digest.Canonical.FromBytes(reconstructed.Bytes())
	assert.Equal(t, expectedDigest, actualDigest,
		"Digest mismatch: compressor=%s, WriteCanonicalTar=%s", expectedDigest, actualDigest)
}

// TestCanonicalTarDeterministicAcrossHeaderVariations verifies that different
// representations of the same logical file produce identical canonical output.
// This is the core property needed for push-after-partial-pull to work:
// the build-time tar and the push-time tar (from filesystem walk) have
// different header formatting, but after canonicalization they must match.
func TestCanonicalTarDeterministicAcrossHeaderVariations(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	content := []byte("file content here")

	// "Archive-style" headers: file type bits in Mode, ./ prefix, Uname set, extra PAX records
	archiveEntries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag:   tar.TypeDir,
				Name:       "./usr/",
				Mode:       0o40755, // file type bits
				Uname:      "root",
				Gname:      "root",
				ModTime:    now,
				AccessTime: now,
				ChangeTime: now,
				PAXRecords: map[string]string{
					"mtime": "1717243200.0",
					"atime": "1717243200.0",
					"ctime": "1717243200.0",
				},
			},
		},
		{
			header: &tar.Header{
				Typeflag:   tar.TypeReg,
				Name:       "./usr/file.txt",
				Mode:       0o100644, // file type bits
				Size:       int64(len(content)),
				Uname:      "root",
				Gname:      "root",
				ModTime:    now,
				AccessTime: now,
				ChangeTime: now,
				PAXRecords: map[string]string{
					"mtime":                "1717243200.0",
					"atime":                "1717243200.0",
					"GOLANG.pkg.something": "value",
				},
			},
			content: content,
		},
	}

	// "Filesystem-style" headers: no file type bits, no ./ prefix, no Uname
	fsEntries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "usr",  // no trailing slash
				Mode:     0o755,  // no file type bits
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/file.txt",
				Mode:     0o644, // no file type bits
				Size:     int64(len(content)),
				ModTime:  now,
			},
			content: content,
		},
	}

	archiveTar := buildTar(t, archiveEntries)
	fsTar := buildTar(t, fsEntries)

	archiveCanonical, _ := canonicalTarFromEntries(t, archiveTar)
	fsCanonical, _ := canonicalTarFromEntries(t, fsTar)

	assert.Equal(t, archiveCanonical, fsCanonical,
		"Archive-style and filesystem-style headers produce different canonical output")

	archiveDigest := digest.Canonical.FromBytes(archiveCanonical)
	fsDigest := digest.Canonical.FromBytes(fsCanonical)
	assert.Equal(t, archiveDigest, fsDigest,
		"Digest mismatch: archive=%s, filesystem=%s", archiveDigest, fsDigest)
}

// TestCanonicalTarWithXattrs verifies xattr round-trip through
// FileMetadata (base64 encoded) and back to tar header.
func TestCanonicalTarWithXattrs(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	capData := "\x01\x00\x00\x02\x00\x20\x00\x00"
	content := []byte("binary")

	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/sbin/ping",
				Mode:     0o755,
				Size:     int64(len(content)),
				ModTime:  now,
				PAXRecords: map[string]string{
					archive.PaxSchilyXattr + "security.capability": capData,
				},
			},
			content: content,
		},
	}

	tarData := buildTar(t, entries)
	canonicalBytes, metadata := canonicalTarFromEntries(t, tarData)

	// Verify xattr survived in metadata
	require.Len(t, metadata, 1)
	require.Contains(t, metadata[0].Xattrs, "security.capability")

	// Reconstruct from TOC
	toc := &TOC{Version: 2, Entries: metadata}
	fg := &mapFileGetter{files: map[string][]byte{"usr/sbin/ping": content}}

	var reconstructed bytes.Buffer
	require.NoError(t, WriteCanonicalTar(toc, fg, &reconstructed))

	assert.Equal(t, canonicalBytes, reconstructed.Bytes(),
		"Xattr round-trip through FileMetadata produced different output")
}

// TestCanonicalTarEmptyFiles verifies that empty files (Size=0) are
// handled correctly in both paths.
func TestCanonicalTarEmptyFiles(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	entries := []testTarEntry{
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
				Name:     "etc/empty",
				Mode:     0o644,
				Size:     0,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "etc/nonempty",
				Mode:     0o644,
				Size:     5,
				ModTime:  now,
			},
			content: []byte("hello"),
		},
	}

	tarData := buildTar(t, entries)
	canonicalBytes, metadata := canonicalTarFromEntries(t, tarData)

	toc := &TOC{Version: 2, Entries: metadata}
	fg := &mapFileGetter{files: map[string][]byte{
		"etc/nonempty": []byte("hello"),
	}}

	var reconstructed bytes.Buffer
	require.NoError(t, WriteCanonicalTar(toc, fg, &reconstructed))

	assert.Equal(t, canonicalBytes, reconstructed.Bytes())
}

// TestCanonicalTarDeviceNodes verifies that device nodes are handled.
func TestCanonicalTarDeviceNodes(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     "dev/",
				Mode:     0o755,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeChar,
				Name:     "dev/null",
				Mode:     0o666,
				Devmajor: 1,
				Devminor: 3,
				ModTime:  now,
			},
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeBlock,
				Name:     "dev/sda",
				Mode:     0o660,
				Devmajor: 8,
				Devminor: 0,
				ModTime:  now,
			},
		},
	}

	tarData := buildTar(t, entries)
	canonicalBytes, metadata := canonicalTarFromEntries(t, tarData)

	toc := &TOC{Version: 2, Entries: metadata}
	fg := &mapFileGetter{files: map[string][]byte{}}

	var reconstructed bytes.Buffer
	require.NoError(t, WriteCanonicalTar(toc, fg, &reconstructed))

	assert.Equal(t, canonicalBytes, reconstructed.Bytes())
}

// TestCanonicalTarFIFO verifies FIFO entries.
func TestCanonicalTarFIFO(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	entries := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeFifo,
				Name:     "tmp/mypipe",
				Mode:     0o644,
				ModTime:  now,
			},
		},
	}

	tarData := buildTar(t, entries)
	canonicalBytes, metadata := canonicalTarFromEntries(t, tarData)

	toc := &TOC{Version: 2, Entries: metadata}
	fg := &mapFileGetter{files: map[string][]byte{}}

	var reconstructed bytes.Buffer
	require.NoError(t, WriteCanonicalTar(toc, fg, &reconstructed))

	assert.Equal(t, canonicalBytes, reconstructed.Bytes())
}

// TestCanonicalTarLinknameNormalization verifies that symlink and hardlink
// targets are also normalized.
func TestCanonicalTarLinknameNormalization(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("data")

	// Build with ./ prefix on linkname
	entries1 := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "./usr/lib/libfoo.so.1",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
			},
			content: content,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "./usr/lib/libfoo.so",
				Linkname: "./usr/lib/libfoo.so.1",
				Mode:     0o777,
				ModTime:  now,
			},
		},
	}

	// Build without ./ prefix
	entries2 := []testTarEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "usr/lib/libfoo.so.1",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
			},
			content: content,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "usr/lib/libfoo.so",
				Linkname: "usr/lib/libfoo.so.1",
				Mode:     0o777,
				ModTime:  now,
			},
		},
	}

	tar1 := buildTar(t, entries1)
	tar2 := buildTar(t, entries2)

	canonical1, _ := canonicalTarFromEntries(t, tar1)
	canonical2, _ := canonicalTarFromEntries(t, tar2)

	assert.Equal(t, canonical1, canonical2,
		"Linkname normalization failed: ./ prefix variants produce different output")
}

// TestNewFileMetadataRoundTrip verifies that NewFileMetadata -> FileMetadataToCanonicalTarHeader
// produces the same header bytes as direct canonicalization.
func TestNewFileMetadataRoundTrip(t *testing.T) {
	now := time.Date(2024, 3, 15, 8, 0, 0, 500000000, time.UTC)

	headers := []*tar.Header{
		{
			Typeflag: tar.TypeDir,
			Name:     "./some/dir/",
			Mode:     0o40755,
			Uid:      1000,
			Gid:      1000,
			Uname:    "user",
			Gname:    "users",
			ModTime:  now,
		},
		{
			Typeflag: tar.TypeReg,
			Name:     "some/dir/file.txt",
			Mode:     0o100644,
			Size:     100,
			Uid:      0,
			Gid:      0,
			ModTime:  now,
			PAXRecords: map[string]string{
				archive.PaxSchilyXattr + "user.test": "value",
			},
		},
		{
			Typeflag: tar.TypeSymlink,
			Name:     "./some/dir/link",
			Linkname: "./some/dir/file.txt",
			Mode:     0o120777,
			ModTime:  now,
		},
		{
			Typeflag: tar.TypeLink,
			Name:     "some/dir/hardlink",
			Linkname: "some/dir/file.txt",
			Mode:     0o644,
			Size:     100, // should be zeroed
			ModTime:  now,
		},
	}

	for _, hdr := range headers {
		t.Run(hdr.Name, func(t *testing.T) {
			// Path 1: direct canonicalization
			directHdr := cloneHeader(hdr)
			directBytes, err := CanonicalHeaderBytes(directHdr)
			require.NoError(t, err)

			// Path 2: canonicalize -> NewFileMetadata -> FileMetadataToCanonicalTarHeader
			canonHdr := cloneHeader(hdr)
			CanonicalizeTarHeader(canonHdr)
			fm, err := NewFileMetadata(canonHdr)
			require.NoError(t, err)

			reconstructedHdr, err := FileMetadataToCanonicalTarHeader(&fm)
			require.NoError(t, err)

			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			require.NoError(t, tw.WriteHeader(reconstructedHdr))
			_ = tw // don't close, just want header bytes
			reconstructedBytes := buf.Bytes()

			assert.Equal(t, directBytes, reconstructedBytes,
				"Round-trip through FileMetadata produced different header bytes for %q", hdr.Name)
		})
	}
}

// TestNewCanonicalTarFilter verifies that piping a non-canonical tar through
// NewCanonicalTarFilter produces the same output as manually canonicalizing
// each entry. This is the mechanism used in Diff() to make filesystem tars
// match the DiffID computed from WriteCanonicalTar.
func TestNewCanonicalTarFilter(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	content := []byte("file content here\n")

	// Build a non-canonical tar (file type bits, ./ prefix, Uname, extra PAX)
	nonCanonicalTar := buildTar(t, []testTarEntry{
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
				Typeflag:   tar.TypeReg,
				Name:       "./usr/file.txt",
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

	// Build the expected canonical tar manually
	expectedCanonical, _ := canonicalTarFromEntries(t, nonCanonicalTar)

	// Run through the filter
	src := io.NopCloser(bytes.NewReader(nonCanonicalTar))
	filtered := NewCanonicalTarFilter(src)
	filteredBytes, err := io.ReadAll(filtered)
	require.NoError(t, err)
	require.NoError(t, filtered.Close())

	expectedDigest := digest.Canonical.FromBytes(expectedCanonical)
	filteredDigest := digest.Canonical.FromBytes(filteredBytes)

	assert.Equal(t, expectedDigest, filteredDigest,
		"Canonical tar filter produced different digest: expected=%s, got=%s",
		expectedDigest, filteredDigest)
	assert.Equal(t, expectedCanonical, filteredBytes,
		"Canonical tar filter produced different bytes")
}

// TestCanonicalTarFilterMatchesWriteCanonicalTar verifies that the filter
// (used in Diff()) produces the same output as WriteCanonicalTar (used in
// ApplyDiff to compute UncompressedDigest). This is the critical property.
func TestCanonicalTarFilterMatchesWriteCanonicalTar(t *testing.T) {
	now := time.Date(2024, 3, 15, 8, 0, 0, 500000000, time.UTC)
	content := []byte("some file content")

	// Build a "filesystem-style" tar (what Diff() produces)
	fsTar := buildTar(t, []testTarEntry{
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
				Name:     "usr/data.bin",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
			},
			content: content,
		},
		{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     "usr/link",
				Linkname: "data.bin",
				Mode:     0o777,
				ModTime:  now,
			},
		},
	})

	// Path 1: Filter (what Diff() will produce)
	src := io.NopCloser(bytes.NewReader(fsTar))
	filtered := NewCanonicalTarFilter(src)
	filteredBytes, err := io.ReadAll(filtered)
	require.NoError(t, err)

	// Path 2: WriteCanonicalTar from metadata (what ApplyDiff computes)
	_, metadata := canonicalTarFromEntries(t, fsTar)

	// Add digest for the regular file
	for i := range metadata {
		if metadata[i].Type == TypeReg && metadata[i].Size > 0 {
			metadata[i].Digest = digest.Canonical.FromBytes(content).String()
		}
	}

	toc := &TOC{Version: 2, Entries: metadata}
	fg := &mapFileGetter{files: map[string][]byte{"usr/data.bin": content}}
	var reconstructed bytes.Buffer
	require.NoError(t, WriteCanonicalTar(toc, fg, &reconstructed))

	// Verify they match
	filteredDigest := digest.Canonical.FromBytes(filteredBytes)
	reconstructedDigest := digest.Canonical.FromBytes(reconstructed.Bytes())

	assert.Equal(t, filteredDigest, reconstructedDigest,
		"Filter digest (%s) != WriteCanonicalTar digest (%s)",
		filteredDigest, reconstructedDigest)
	assert.Equal(t, filteredBytes, reconstructed.Bytes(),
		"Filter output != WriteCanonicalTar output")
}

// cloneHeader makes a deep copy of a tar.Header.
func cloneHeader(hdr *tar.Header) *tar.Header {
	clone := *hdr
	if hdr.PAXRecords != nil {
		clone.PAXRecords = make(map[string]string, len(hdr.PAXRecords))
		for k, v := range hdr.PAXRecords {
			clone.PAXRecords[k] = v
		}
	}
	return &clone
}
