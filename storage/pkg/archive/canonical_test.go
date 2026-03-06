package archive

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCanonicalTarFilterProducesDeterministicOutput verifies that the
// canonical tar filter produces the same output regardless of input
// header variations (file type bits, ./ prefix, Uname/Gname, extra PAX records).
func TestCanonicalTarFilterProducesDeterministicOutput(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	content := []byte("file content here\n")

	// Build an "archive-style" tar
	archiveTar := buildTestTar(t, []testEntry{
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
					"mtime":                          "1717243200.0",
					PaxSchilyXattr + "user.myxattr": "myvalue",
				},
			},
			content: content,
		},
	})

	// Build a "filesystem-style" tar
	fsTar := buildTestTar(t, []testEntry{
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
				Typeflag: tar.TypeReg,
				Name:     "usr/file.txt",
				Mode:     0o644,
				Size:     int64(len(content)),
				ModTime:  now,
				PAXRecords: map[string]string{
					PaxSchilyXattr + "user.myxattr": "myvalue",
				},
			},
			content: content,
		},
	})

	// Filter both
	filtered1 := filterTar(t, archiveTar)
	filtered2 := filterTar(t, fsTar)

	d1 := digest.Canonical.FromBytes(filtered1)
	d2 := digest.Canonical.FromBytes(filtered2)

	assert.Equal(t, d1, d2,
		"Archive-style and filesystem-style tars produce different digests after filtering: %s vs %s", d1, d2)
	assert.Equal(t, filtered1, filtered2,
		"Archive-style and filesystem-style tars produce different bytes after filtering")
}

// TestCanonicalTarFilterRoundTrip verifies that filtering twice produces
// the same output as filtering once (idempotency).
func TestCanonicalTarFilterRoundTrip(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("data")

	rawTar := buildTestTar(t, []testEntry{
		{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     "./file.txt",
				Mode:     0o100644,
				Size:     int64(len(content)),
				Uname:    "user",
				Gname:    "group",
				ModTime:  now,
			},
			content: content,
		},
	})

	once := filterTar(t, rawTar)
	twice := filterTar(t, once)

	assert.Equal(t, once, twice, "Canonical tar filter is not idempotent")
}

type testEntry struct {
	header  *tar.Header
	content []byte
}

func buildTestTar(t *testing.T, entries []testEntry) []byte {
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

func filterTar(t *testing.T, data []byte) []byte {
	t.Helper()
	src := io.NopCloser(bytes.NewReader(data))
	filtered := NewCanonicalTarFilter(src)
	result, err := io.ReadAll(filtered)
	require.NoError(t, err)
	return result
}

// mapFileGetter implements FileGetter using an in-memory map.
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

// TestWriteCanonicalTarBasic verifies that WriteCanonicalTar
// (which reads a TOC JSON and produces a canonical tar) produces
// valid, parseable output.
func TestWriteCanonicalTarBasic(t *testing.T) {
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	content1 := []byte("hello world content\n")
	content2 := []byte("another file\n")

	toc := CanonicalTOC{
		Version: 2,
		Entries: []CanonicalTOCEntry{
			{Type: "dir", Name: "usr/", Mode: 0o755, ModTime: &now},
			{Type: "dir", Name: "usr/share/", Mode: 0o755, ModTime: &now},
			{Type: "reg", Name: "usr/share/doc.txt", Mode: 0o644, Size: int64(len(content1)), ModTime: &now,
				Xattrs: map[string]string{
					"security.capability": base64.StdEncoding.EncodeToString([]byte("\x01\x00\x00\x02\x00\x20\x00\x00")),
				}},
			{Type: "reg", Name: "usr/share/readme", Mode: 0o644, Size: int64(len(content2)), ModTime: &now},
			{Type: "symlink", Name: "usr/share/link", Linkname: "doc.txt", Mode: 0o777, ModTime: &now},
		},
	}

	tocJSON, err := json.Marshal(toc)
	require.NoError(t, err)

	fg := &mapFileGetter{files: map[string][]byte{
		"usr/share/doc.txt": content1,
		"usr/share/readme":  content2,
	}}

	var buf bytes.Buffer
	err = WriteCanonicalTar(bytes.NewReader(tocJSON), fg, &buf)
	require.NoError(t, err)

	// Parse the output and verify
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)

		// Verify canonical properties
		assert.Equal(t, "", hdr.Uname, "Uname should be empty for %s", hdr.Name)
		assert.Equal(t, "", hdr.Gname, "Gname should be empty for %s", hdr.Name)
		assert.Contains(t, hdr.PAXRecords, "CONTAINERS.canonical")
	}

	assert.Equal(t, []string{
		"usr/", "usr/share/", "usr/share/doc.txt", "usr/share/readme", "usr/share/link",
	}, names)
}

// TestWriteCanonicalTarIdempotent verifies that WriteCanonicalTar
// produces the same output every time for the same input.
func TestWriteCanonicalTarIdempotent(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	content := []byte("test content")

	toc := CanonicalTOC{
		Version: 2,
		Entries: []CanonicalTOCEntry{
			{Type: "reg", Name: "file.txt", Mode: 0o644, Size: int64(len(content)), ModTime: &now},
		},
	}
	tocJSON, err := json.Marshal(toc)
	require.NoError(t, err)

	fg := &mapFileGetter{files: map[string][]byte{"file.txt": content}}

	var buf1, buf2 bytes.Buffer
	require.NoError(t, WriteCanonicalTar(bytes.NewReader(tocJSON), fg, &buf1))
	require.NoError(t, WriteCanonicalTar(bytes.NewReader(tocJSON), fg, &buf2))

	d1 := digest.Canonical.FromBytes(buf1.Bytes())
	d2 := digest.Canonical.FromBytes(buf2.Bytes())
	assert.Equal(t, d1, d2, "WriteCanonicalTar is not deterministic")
	assert.Equal(t, buf1.Bytes(), buf2.Bytes())
}

// TestWriteCanonicalTarMatchesFilterForSameModTime verifies that
// WriteCanonicalTar and NewCanonicalTarFilter produce identical output
// when given the same modtime precision (second-granularity, typical
// of USTAR input tars).
func TestWriteCanonicalTarMatchesFilterForSameModTime(t *testing.T) {
	// Use second-precision modtime (what filesystem tars typically have)
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	content := []byte("hello world content\n")

	// Build a tar and filter it
	inputTar := buildTestTar(t, []testEntry{
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
				PAXRecords: map[string]string{
					PaxSchilyXattr + "security.capability": "\x01\x00\x00\x02",
				},
			},
			content: content,
		},
	})
	filteredBytes := filterTar(t, inputTar)

	// Build equivalent TOC (with same second-precision modtime)
	toc := CanonicalTOC{
		Version: 2,
		Entries: []CanonicalTOCEntry{
			{Type: "dir", Name: "usr/", Mode: 0o755, ModTime: &now},
			{Type: "reg", Name: "usr/file.txt", Mode: 0o644, Size: int64(len(content)), ModTime: &now,
				Xattrs: map[string]string{
					"security.capability": base64.StdEncoding.EncodeToString([]byte("\x01\x00\x00\x02")),
				}},
		},
	}
	tocJSON, err := json.Marshal(toc)
	require.NoError(t, err)

	fg := &mapFileGetter{files: map[string][]byte{"usr/file.txt": content}}
	var reconstructed bytes.Buffer
	require.NoError(t, WriteCanonicalTar(bytes.NewReader(tocJSON), fg, &reconstructed))

	filteredDigest := digest.Canonical.FromBytes(filteredBytes)
	reconstructedDigest := digest.Canonical.FromBytes(reconstructed.Bytes())

	assert.Equal(t, filteredDigest, reconstructedDigest,
		"NewCanonicalTarFilter digest (%s) != WriteCanonicalTar digest (%s)",
		filteredDigest, reconstructedDigest)
	assert.Equal(t, filteredBytes, reconstructed.Bytes(),
		"NewCanonicalTarFilter output != WriteCanonicalTar output (len: %d vs %d)",
		len(filteredBytes), len(reconstructed.Bytes()))
}
