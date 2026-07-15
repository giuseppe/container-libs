package archive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalGlobalHeader(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)
	require.NoError(t, cw.Close())

	data := buf.Bytes()
	// Global header (512 bytes) + pax data block (512 bytes) + 2 zero blocks
	require.True(t, len(data) >= 4*blockSize, "expected at least 4 blocks, got %d bytes", len(data))

	// Check global header typeflag
	assert.Equal(t, byte('g'), data[156], "global header typeflag should be 'g'")

	// Check magic
	assert.Equal(t, "ustar\x00", string(data[257:263]), "magic should be 'ustar\\0'")
	assert.Equal(t, "00", string(data[263:265]), "version should be '00'")

	// Extract pax data size from header
	sizeStr := strings.TrimRight(string(data[124:136]), "\x00 ")
	var size int64
	fmt.Sscanf(sizeStr, "%o", &size)

	// Check that pax data contains canonical-tar=1
	paxData := string(data[blockSize : blockSize+size])
	assert.Contains(t, paxData, "canonical-tar=1")
}

func TestCanonicalShortPath(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	hdr := &tar.Header{
		Name:     "./usr/bin/hello",
		Mode:     0o755,
		Typeflag: tar.TypeReg,
		Size:     5,
	}
	require.NoError(t, cw.WriteHeader(hdr))
	_, err := cw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, cw.Close())

	// Parse back with standard tar reader
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// First entry should be the global header
	gh, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, byte(tar.TypeXGlobalHeader), gh.Typeflag)

	// Second entry should be our file
	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./usr/bin/hello", entry.Name)
	assert.Equal(t, int64(0o755), entry.Mode)
	assert.Equal(t, int64(5), entry.Size)

	content, err := io.ReadAll(tr)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
}

func TestCanonicalPrefixFieldAlwaysEmpty(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	// Path that would trigger prefix/name split in Go's tar.Writer
	// (between 100 and 255 bytes with a suitable '/' separator)
	longDir := strings.Repeat("a", 80)
	longPath := fmt.Sprintf("./%s/file.txt", longDir)

	hdr := &tar.Header{
		Name:     longPath,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
	}
	require.NoError(t, cw.WriteHeader(hdr))
	require.NoError(t, cw.Close())

	data := buf.Bytes()

	// Skip global header (512) + global pax data (512) = 1024
	// Then find the entry headers. Since path > 100 bytes, there should be
	// a pax extended header followed by the ustar header.

	// Find the ustar header for our entry (look for typeflag '0')
	var entryOffset int
	for i := blockSize * 2; i < len(data)-blockSize; i += blockSize {
		if data[i+156] == '0' { // TypeReg
			entryOffset = i
			break
		}
	}
	require.NotZero(t, entryOffset, "could not find entry header")

	// Check that prefix field (bytes 345-500) is all zeros
	prefix := data[entryOffset+345 : entryOffset+500]
	for i, b := range prefix {
		assert.Equal(t, byte(0), b, "prefix byte %d should be zero, got %d", i, b)
	}
}

func TestCanonicalLongPathPAXRecord(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	// Path longer than 100 bytes
	longPath := "./" + strings.Repeat("subdir/", 15) + "file.txt"
	require.True(t, len(longPath) > 100)

	hdr := &tar.Header{
		Name:     longPath,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
	}
	require.NoError(t, cw.WriteHeader(hdr))
	require.NoError(t, cw.Close())

	// Parse back — standard tar reader should reconstruct full path from pax
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err := tr.Next()
	require.NoError(t, err)

	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, longPath, entry.Name)
}

func TestCanonicalHardlink(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./file1",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     3,
	}))
	_, err := cw.Write([]byte("foo"))
	require.NoError(t, err)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./file2",
		Typeflag: tar.TypeLink,
		Linkname: "./file1",
	}))

	require.NoError(t, cw.Close())

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err = tr.Next()
	require.NoError(t, err)

	entry1, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./file1", entry1.Name)
	assert.Equal(t, byte(tar.TypeReg), entry1.Typeflag)

	entry2, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./file2", entry2.Name)
	assert.Equal(t, byte(tar.TypeLink), entry2.Typeflag)
	assert.Equal(t, "./file1", entry2.Linkname)
}

func TestCanonicalWhiteout(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./.wh.removed",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
	}))

	require.NoError(t, cw.Close())

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err := tr.Next()
	require.NoError(t, err)

	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./.wh.removed", entry.Name)
	assert.Equal(t, int64(0o644), entry.Mode)
	assert.Equal(t, 0, entry.Uid)
	assert.Equal(t, 0, entry.Gid)
}

func TestCanonicalOctalEncoding(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./test",
		Mode:     0o755,
		Uid:      1000,
		Gid:      1000,
		Typeflag: tar.TypeReg,
		Size:     0,
		ModTime:  time.Unix(1700000000, 0),
	}))

	require.NoError(t, cw.Close())

	data := buf.Bytes()

	// Find the entry header (after global header blocks)
	var entryOffset int
	for i := blockSize * 2; i < len(data)-blockSize; i += blockSize {
		if data[i+156] == '0' { // TypeReg
			entryOffset = i
			break
		}
	}
	require.NotZero(t, entryOffset)

	// mode field: bytes 100-108, should be "0000755\0"
	assert.Equal(t, "0000755\x00", string(data[entryOffset+100:entryOffset+108]))

	// uid field: bytes 108-116, should be "0001750\0" (1000 octal)
	assert.Equal(t, "0001750\x00", string(data[entryOffset+108:entryOffset+116]))

	// gid field: bytes 116-124, should be "0001750\0" (1000 octal)
	assert.Equal(t, "0001750\x00", string(data[entryOffset+116:entryOffset+124]))

	// uname field: bytes 265-297, should be all zeros
	for i := 265; i < 297; i++ {
		assert.Equal(t, byte(0), data[entryOffset+i], "uname byte %d should be zero", i-265)
	}

	// gname field: bytes 297-329, should be all zeros
	for i := 297; i < 329; i++ {
		assert.Equal(t, byte(0), data[entryOffset+i], "gname byte %d should be zero", i-297)
	}
}

func TestCanonicalDirectory(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./usr/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}))

	require.NoError(t, cw.Close())

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err := tr.Next()
	require.NoError(t, err)

	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./usr/", entry.Name)
	assert.Equal(t, byte(tar.TypeDir), entry.Typeflag)
	assert.Equal(t, int64(0), entry.Size)
}

func TestCanonicalSymlink(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./usr/bin/python3",
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
		Linkname: "python3.11",
	}))

	require.NoError(t, cw.Close())

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err := tr.Next()
	require.NoError(t, err)

	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./usr/bin/python3", entry.Name)
	assert.Equal(t, byte(tar.TypeSymlink), entry.Typeflag)
	assert.Equal(t, "python3.11", entry.Linkname)
	assert.Equal(t, int64(0), entry.Size)
}

func TestCanonicalXattrs(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./test",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
		PAXRecords: map[string]string{
			"SCHILY.xattr.security.selinux": "system_u:object_r:usr_t:s0\x00",
			"SCHILY.xattr.user.foo":         "bar",
		},
	}))

	require.NoError(t, cw.Close())

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err := tr.Next()
	require.NoError(t, err)

	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./test", entry.Name)
	// Xattrs should be preserved via PAXRecords
	assert.Contains(t, entry.PAXRecords, "SCHILY.xattr.security.selinux")
	assert.Contains(t, entry.PAXRecords, "SCHILY.xattr.user.foo")
	// Check sorted order: security.selinux should come before user.foo
	assert.Equal(t, "system_u:object_r:usr_t:s0\x00", entry.PAXRecords["SCHILY.xattr.security.selinux"])
	assert.Equal(t, "bar", entry.PAXRecords["SCHILY.xattr.user.foo"])
}

func TestCanonicalPAXRecordOrder(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	// Create a header that triggers multiple pax records:
	// large uid, sub-second mtime, and xattrs
	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./test",
		Mode:     0o644,
		Uid:      3000000, // > 2097151, needs pax
		Gid:      3000000, // > 2097151, needs pax
		Typeflag: tar.TypeReg,
		Size:     0,
		ModTime:  time.Unix(1700000000, 123456789),
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.test": "value",
		},
	}))

	require.NoError(t, cw.Close())

	data := buf.Bytes()

	// Find the pax extended header for our entry
	var paxOffset int
	for i := blockSize * 2; i < len(data)-blockSize; i += blockSize {
		if data[i+156] == 'x' { // TypeXHeader
			paxOffset = i
			break
		}
	}
	require.NotZero(t, paxOffset, "could not find pax extended header")

	// Read pax data size
	sizeStr := strings.TrimRight(string(data[paxOffset+124:paxOffset+136]), "\x00 ")
	var size int64
	fmt.Sscanf(sizeStr, "%o", &size)

	paxData := string(data[paxOffset+blockSize : paxOffset+blockSize+int(size)])

	// Verify prescribed order: uid, gid, mtime, SCHILY.xattr.*
	uidIdx := strings.Index(paxData, "uid=")
	gidIdx := strings.Index(paxData, "gid=")
	mtimeIdx := strings.Index(paxData, "mtime=")
	xattrIdx := strings.Index(paxData, "SCHILY.xattr.")

	require.NotEqual(t, -1, uidIdx, "uid record not found")
	require.NotEqual(t, -1, gidIdx, "gid record not found")
	require.NotEqual(t, -1, mtimeIdx, "mtime record not found")
	require.NotEqual(t, -1, xattrIdx, "xattr record not found")

	assert.Less(t, uidIdx, gidIdx, "uid should come before gid")
	assert.Less(t, gidIdx, mtimeIdx, "gid should come before mtime")
	assert.Less(t, mtimeIdx, xattrIdx, "mtime should come before SCHILY.xattr")
}

func TestCanonicalSubSecondMtime(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	ts := time.Unix(1700000000, 123456789)
	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./test",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
		ModTime:  ts,
	}))

	require.NoError(t, cw.Close())

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global header
	_, err := tr.Next()
	require.NoError(t, err)

	entry, err := tr.Next()
	require.NoError(t, err)

	// The sub-second mtime should be preserved via pax record
	assert.Equal(t, ts.Unix(), entry.ModTime.Unix())
	assert.Equal(t, ts.Nanosecond(), entry.ModTime.Nanosecond())
}

func TestCanonicalHeaderBytes(t *testing.T) {
	hdr := &tar.Header{
		Name:     "./usr/bin/hello",
		Mode:     0o755,
		Typeflag: tar.TypeReg,
		Size:     100,
	}
	headerBytes, err := CanonicalHeaderBytes(hdr)
	require.NoError(t, err)
	require.NotEmpty(t, headerBytes)

	// Should be a multiple of 512
	assert.Equal(t, 0, len(headerBytes)%blockSize)

	// Should NOT contain the global header (no 'g' typeflag)
	assert.Equal(t, byte('0'), headerBytes[156])

	// The header bytes should be parseable by a standard tar reader
	tr := tar.NewReader(bytes.NewReader(headerBytes))
	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./usr/bin/hello", entry.Name)
	assert.Equal(t, int64(0o755), entry.Mode)
}

func TestCanonicalHeaderBytesLongPath(t *testing.T) {
	longPath := "./" + strings.Repeat("subdir/", 15) + "file.txt"
	require.True(t, len(longPath) > 100)

	hdr := &tar.Header{
		Name:     longPath,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
	}
	headerBytes, err := CanonicalHeaderBytes(hdr)
	require.NoError(t, err)

	// Should include pax extended header + ustar header
	require.True(t, len(headerBytes) > blockSize)

	// Parse back
	tr := tar.NewReader(bytes.NewReader(headerBytes))
	entry, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, longPath, entry.Name)
}

func TestWriteHeaderOnly(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriterRaw(&buf)

	// WriteHeaderOnly should not require content to follow
	require.NoError(t, cw.WriteHeaderOnly(&tar.Header{
		Name:     "./file1",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     100,
	}))

	// Should be able to write another header immediately
	require.NoError(t, cw.WriteHeaderOnly(&tar.Header{
		Name:     "./file2",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     200,
	}))

	require.NoError(t, cw.Close())
}

func TestCanonicalRawWriter(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriterRaw(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./test",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     0,
	}))

	require.NoError(t, cw.Close())

	// Raw writer should NOT have a global header.
	// First entry should be our file directly.
	data := buf.Bytes()
	assert.Equal(t, byte('0'), data[156], "first block should be a regular file entry, not global header")
}

func TestFormatPAXRecord(t *testing.T) {
	tests := []struct {
		key, value, expected string
	}{
		{"path", "short", "14 path=short\n"},
		{"uid", "1000", "12 uid=1000\n"},
		{"canonical-tar", "1", "19 canonical-tar=1\n"},
	}

	for _, tc := range tests {
		result := formatPAXRecord(tc.key, tc.value)
		assert.Equal(t, tc.expected, result, "formatPAXRecord(%q, %q)", tc.key, tc.value)

		// Verify self-consistency: length prefix equals record length
		var length int
		fmt.Sscanf(result, "%d ", &length)
		assert.Equal(t, length, len(result), "length prefix mismatch for %q", result)
	}
}

func TestCanonicalFormatOctal(t *testing.T) {
	tests := []struct {
		value    int64
		size     int
		expected string
	}{
		{0, 8, "0000000\x00"},
		{0o755, 8, "0000755\x00"},
		{1000, 8, "0001750\x00"},
		{0, 12, "00000000000\x00"},
	}

	for _, tc := range tests {
		b := make([]byte, tc.size)
		canonicalFormatOctal(b, tc.value)
		assert.Equal(t, tc.expected, string(b), "canonicalFormatOctal(%d, size=%d)", tc.value, tc.size)
	}
}

func TestCanonicalEndOfArchive(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)
	require.NoError(t, cw.Close())

	data := buf.Bytes()
	// Last 1024 bytes should be all zeros (two zero blocks)
	endBlocks := data[len(data)-2*blockSize:]
	for i, b := range endBlocks {
		assert.Equal(t, byte(0), b, "end-of-archive byte %d should be zero", i)
	}
}

func TestCanonicalMultipleFiles(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCanonicalTarWriter(&buf)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}))

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./a",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     1,
	}))
	_, err := cw.Write([]byte("a"))
	require.NoError(t, err)

	require.NoError(t, cw.WriteHeader(&tar.Header{
		Name:     "./b",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
		Size:     1,
	}))
	_, err = cw.Write([]byte("b"))
	require.NoError(t, err)

	require.NoError(t, cw.Close())

	// Verify round-trip
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))

	// Skip global
	_, err = tr.Next()
	require.NoError(t, err)

	e1, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./", e1.Name)

	e2, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./a", e2.Name)

	e3, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, "./b", e3.Name)

	_, err = tr.Next()
	assert.Equal(t, io.EOF, err)
}

