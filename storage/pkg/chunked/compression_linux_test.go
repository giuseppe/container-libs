package chunked

import (
	archivetar "archive/tar"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/pgzip"
	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vbatts/tar-split/archive/tar"
	"github.com/vbatts/tar-split/tar/asm"
	"github.com/vbatts/tar-split/tar/storage"
	"go.podman.io/storage/pkg/archive"
	"go.podman.io/storage/pkg/chunked/compressor"
	"go.podman.io/storage/pkg/chunked/internal/minimal"
)

func TestTarSizeFromTarSplit(t *testing.T) {
	var tarball bytes.Buffer
	tarWriter := tar.NewWriter(&tarball)
	for _, e := range someFiles {
		tf, err := typeToTarType(e.Type)
		require.NoError(t, err)
		err = tarWriter.WriteHeader(&tar.Header{
			Typeflag: tf,
			Name:     e.Name,
			Size:     e.Size,
			Mode:     e.Mode,
		})
		require.NoError(t, err)
		data := make([]byte, e.Size)
		_, err = tarWriter.Write(data)
		require.NoError(t, err)
	}
	err := tarWriter.Close()
	require.NoError(t, err)
	expectedTarSize := int64(tarball.Len())

	var tarSplit bytes.Buffer
	tsReader, done, err := asm.NewInputTarStreamWithDone(&tarball, storage.NewJSONPacker(&tarSplit), storage.NewDiscardFilePutter())
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, tsReader)
	require.NoError(t, err)
	require.NoError(t, tsReader.Close())
	require.NoError(t, <-done)

	res, err := tarSizeFromTarSplit(&tarSplit)
	require.NoError(t, err)
	assert.Equal(t, expectedTarSize, res)
}

func TestFileMetadataToTarHeader(t *testing.T) {
	modTime := time.Unix(1700000000, 123456789)
	fm := &minimal.FileMetadata{
		Type:     "reg",
		Name:     "./usr/bin/hello",
		Linkname: "",
		Mode:     0o755,
		Size:     100,
		UID:      1000,
		GID:      1000,
		ModTime:  &modTime,
		Xattrs: map[string]string{
			"security.selinux": base64.StdEncoding.EncodeToString([]byte("system_u:object_r:usr_t:s0\x00")),
		},
	}

	hdr, err := fileMetadataToTarHeader(fm)
	require.NoError(t, err)
	assert.Equal(t, byte(archivetar.TypeReg), hdr.Typeflag)
	assert.Equal(t, "./usr/bin/hello", hdr.Name)
	assert.Equal(t, int64(0o755), hdr.Mode)
	assert.Equal(t, int64(100), hdr.Size)
	assert.Equal(t, 1000, hdr.Uid)
	assert.Equal(t, 1000, hdr.Gid)
	assert.Equal(t, modTime, hdr.ModTime)
	assert.Equal(t, "system_u:object_r:usr_t:s0\x00", hdr.PAXRecords["SCHILY.xattr.security.selinux"])
}

func TestFileMetadataToTarHeaderRoundtrip(t *testing.T) {
	modTime := time.Unix(1700000000, 0)
	origHdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "./usr/bin/hello",
		Mode:     0o755,
		Size:     100,
		Uid:      1000,
		Gid:      1000,
		ModTime:  modTime,
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.test": "value",
		},
	}

	fm, err := minimal.NewFileMetadata(origHdr)
	require.NoError(t, err)

	hdr, err := fileMetadataToTarHeader(&fm)
	require.NoError(t, err)

	assert.Equal(t, origHdr.Name, hdr.Name)
	assert.Equal(t, origHdr.Mode, hdr.Mode)
	assert.Equal(t, origHdr.Size, hdr.Size)
	assert.Equal(t, origHdr.Uid, hdr.Uid)
	assert.Equal(t, origHdr.Gid, hdr.Gid)
	assert.Equal(t, origHdr.ModTime.Unix(), hdr.ModTime.Unix())
	assert.Equal(t, origHdr.PAXRecords["SCHILY.xattr.user.test"], hdr.PAXRecords["SCHILY.xattr.user.test"])
}

func TestTarSizeFromTOC(t *testing.T) {
	// Build a canonical tar archive
	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./",
		Mode:     0o755,
		Typeflag: archivetar.TypeDir,
	}))

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./hello",
		Mode:     0o644,
		Typeflag: archivetar.TypeReg,
		Size:     5,
	}))
	_, err := cw.Write([]byte("hello"))
	require.NoError(t, err)

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./link",
		Typeflag: archivetar.TypeSymlink,
		Linkname: "hello",
	}))

	require.NoError(t, cw.Close())

	expectedSize := int64(canonicalTar.Len())

	// Build a TOC from the same entries
	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries: []minimal.FileMetadata{
			{Type: "dir", Name: "./", Mode: 0o755},
			{Type: "reg", Name: "./hello", Mode: 0o644, Size: 5},
			{Type: "symlink", Name: "./link", Linkname: "hello"},
		},
	}

	size, err := tarSizeFromTOC(toc)
	require.NoError(t, err)
	assert.Equal(t, expectedSize, size)
}

type memFileGetter struct {
	files map[string][]byte
}

func (m *memFileGetter) Get(filename string) (io.ReadCloser, error) {
	data, ok := m.files[filename]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", filename)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestGenerateTarSplitFromTOC(t *testing.T) {
	// Build a canonical tar archive
	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./",
		Mode:     0o755,
		Typeflag: archivetar.TypeDir,
	}))

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./hello",
		Mode:     0o644,
		Typeflag: archivetar.TypeReg,
		Size:     5,
	}))
	_, err := cw.Write([]byte("hello"))
	require.NoError(t, err)

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./empty",
		Mode:     0o644,
		Typeflag: archivetar.TypeReg,
		Size:     0,
	}))

	require.NoError(t, cw.Close())

	originalBytes := canonicalTar.Bytes()

	// Build TOC
	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries: []minimal.FileMetadata{
			{Type: "dir", Name: "./", Mode: 0o755},
			{Type: "reg", Name: "./hello", Mode: 0o644, Size: 5},
			{Type: "reg", Name: "./empty", Mode: 0o644, Size: 0},
		},
	}

	fg := &memFileGetter{
		files: map[string][]byte{
			"./hello": []byte("hello"),
		},
	}

	tmpDir := t.TempDir()
	tarSplitFile, err := generateTarSplitFromTOC(toc, fg, tmpDir)
	require.NoError(t, err)
	defer tarSplitFile.Close()

	// Now reassemble the tar using WriteOutputTarStream and verify it matches
	metadata := storage.NewJSONUnpacker(tarSplitFile)
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes(), "reassembled tar should match original canonical tar")
}

func TestGenerateTarSplitFromTOCWithXattrs(t *testing.T) {
	// Build a canonical tar archive with xattrs
	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)

	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name:     "./test",
		Mode:     0o644,
		Typeflag: archivetar.TypeReg,
		Size:     3,
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.test": "value",
		},
	}))
	_, err := cw.Write([]byte("foo"))
	require.NoError(t, err)

	require.NoError(t, cw.Close())

	originalBytes := canonicalTar.Bytes()

	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries: []minimal.FileMetadata{
			{
				Type: "reg",
				Name: "./test",
				Mode: 0o644,
				Size: 3,
				Xattrs: map[string]string{
					"user.test": base64.StdEncoding.EncodeToString([]byte("value")),
				},
			},
		},
	}

	fg := &memFileGetter{
		files: map[string][]byte{
			"./test": []byte("foo"),
		},
	}

	tmpDir := t.TempDir()
	tarSplitFile, err := generateTarSplitFromTOC(toc, fg, tmpDir)
	require.NoError(t, err)
	defer tarSplitFile.Close()

	metadata := storage.NewJSONUnpacker(tarSplitFile)
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes())
}

func TestGenerateTarSplitFromTOCRealistic(t *testing.T) {
	modTime := time.Unix(1700000000, 0)
	entries := []archivetar.Header{
		{Name: "./", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/bin/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/bin/hello", Mode: 0o755, Typeflag: archivetar.TypeReg, Size: 5, Uid: 0, Gid: 0, ModTime: modTime},
		{Name: "./usr/bin/world", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 3, Uid: 1000, Gid: 1000, ModTime: modTime},
		{Name: "./usr/bin/empty", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 0, ModTime: modTime},
		{Name: "./usr/bin/link", Typeflag: archivetar.TypeSymlink, Linkname: "hello", ModTime: modTime},
		{Name: "./usr/bin/hardlink", Typeflag: archivetar.TypeLink, Linkname: "./usr/bin/hello", ModTime: modTime},
		{Name: "./etc/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./etc/config", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 7, ModTime: modTime,
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.selinux": "system_u:object_r:usr_t:s0\x00",
				"SCHILY.xattr.user.test":        "value",
			},
		},
	}

	fileContents := map[string][]byte{
		"./usr/bin/hello": []byte("hello"),
		"./usr/bin/world": []byte("hey"),
		"./etc/config":    []byte("foo=bar"),
	}

	// Build canonical tar
	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)
	for _, e := range entries {
		require.NoError(t, cw.WriteHeader(&e))
		if data, ok := fileContents[e.Name]; ok {
			_, err := cw.Write(data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, cw.Close())
	originalBytes := canonicalTar.Bytes()

	// Build TOC using NewFileMetadata (the real production path)
	var tocEntries []minimal.FileMetadata
	tr := tar.NewReader(bytes.NewReader(originalBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		// Skip global pax header
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		fm, err := minimal.NewFileMetadata(hdr)
		require.NoError(t, err)
		tocEntries = append(tocEntries, fm)
	}

	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries:      tocEntries,
	}

	fg := &memFileGetter{files: fileContents}

	tmpDir := t.TempDir()
	tarSplitFile, err := generateTarSplitFromTOC(toc, fg, tmpDir)
	require.NoError(t, err)
	defer tarSplitFile.Close()

	metadata := storage.NewJSONUnpacker(tarSplitFile)
	fg2 := &memFileGetter{files: fileContents}
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg2, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes(), "reassembled tar should match original canonical tar")
}

func TestGenerateTarSplitFromTOCLongPaths(t *testing.T) {
	modTime := time.Unix(1700000000, 0)
	longDir := "./" + strings.Repeat("subdir/", 15) // > 100 chars
	longFile := longDir + "file.txt"

	entries := []archivetar.Header{
		{Name: longDir, Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: longFile, Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 4, ModTime: modTime},
	}

	fileContents := map[string][]byte{
		longFile: []byte("data"),
	}

	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)
	for _, e := range entries {
		require.NoError(t, cw.WriteHeader(&e))
		if data, ok := fileContents[e.Name]; ok {
			_, err := cw.Write(data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, cw.Close())
	originalBytes := canonicalTar.Bytes()

	var tocEntries []minimal.FileMetadata
	tr := tar.NewReader(bytes.NewReader(originalBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		fm, err := minimal.NewFileMetadata(hdr)
		require.NoError(t, err)
		tocEntries = append(tocEntries, fm)
	}

	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries:      tocEntries,
	}

	fg := &memFileGetter{files: fileContents}
	tmpDir := t.TempDir()
	tarSplitFile, err := generateTarSplitFromTOC(toc, fg, tmpDir)
	require.NoError(t, err)
	defer tarSplitFile.Close()

	metadata := storage.NewJSONUnpacker(tarSplitFile)
	fg2 := &memFileGetter{files: fileContents}
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg2, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes(), "reassembled tar with long paths should match original")
}

func TestGenerateTarSplitFromTOCSubSecondMtime(t *testing.T) {
	ts := time.Unix(1700000000, 123456789)
	entries := []archivetar.Header{
		{Name: "./", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: ts},
		{Name: "./file", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 3, ModTime: ts},
	}

	fileContents := map[string][]byte{
		"./file": []byte("abc"),
	}

	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)
	for _, e := range entries {
		require.NoError(t, cw.WriteHeader(&e))
		if data, ok := fileContents[e.Name]; ok {
			_, err := cw.Write(data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, cw.Close())
	originalBytes := canonicalTar.Bytes()

	var tocEntries []minimal.FileMetadata
	tr := tar.NewReader(bytes.NewReader(originalBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		fm, err := minimal.NewFileMetadata(hdr)
		require.NoError(t, err)
		tocEntries = append(tocEntries, fm)
	}

	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries:      tocEntries,
	}

	fg := &memFileGetter{files: fileContents}
	tmpDir := t.TempDir()
	tarSplitFile, err := generateTarSplitFromTOC(toc, fg, tmpDir)
	require.NoError(t, err)
	defer tarSplitFile.Close()

	metadata := storage.NewJSONUnpacker(tarSplitFile)
	fg2 := &memFileGetter{files: fileContents}
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg2, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes(), "reassembled tar with sub-second mtime should match original")
}

func TestGenerateTarSplitFromTOCLargeUidGid(t *testing.T) {
	modTime := time.Unix(1700000000, 0)
	entries := []archivetar.Header{
		{Name: "./", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./file", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 3, Uid: 3000000, Gid: 3000000, ModTime: modTime},
	}

	fileContents := map[string][]byte{
		"./file": []byte("abc"),
	}

	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)
	for _, e := range entries {
		require.NoError(t, cw.WriteHeader(&e))
		if data, ok := fileContents[e.Name]; ok {
			_, err := cw.Write(data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, cw.Close())
	originalBytes := canonicalTar.Bytes()

	var tocEntries []minimal.FileMetadata
	tr := tar.NewReader(bytes.NewReader(originalBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		fm, err := minimal.NewFileMetadata(hdr)
		require.NoError(t, err)
		tocEntries = append(tocEntries, fm)
	}

	toc := &minimal.TOC{
		Version:      1,
		CanonicalTar: true,
		Entries:      tocEntries,
	}

	fg := &memFileGetter{files: fileContents}
	tmpDir := t.TempDir()
	tarSplitFile, err := generateTarSplitFromTOC(toc, fg, tmpDir)
	require.NoError(t, err)
	defer tarSplitFile.Close()

	metadata := storage.NewJSONUnpacker(tarSplitFile)
	fg2 := &memFileGetter{files: fileContents}
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg2, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes(), "reassembled tar with large uid/gid should match original")
}

// TestBuildCommitPushRoundtrip simulates the full podman build → push pipeline:
// 1. overlay.Diff() generates canonical tar during commit
// 2. Buildah compresses with zstd:chunked
// 3. applyDiff decompresses, computes UncompressedDigest, creates tar-split
// 4. During push, Diff() reconstructs tar from tar-split
// 5. Verify reconstructed tar matches UncompressedDigest
func TestBuildCommitPushRoundtrip(t *testing.T) {
	modTime := time.Unix(1700000000, 123456789)

	entries := []archivetar.Header{
		{Name: "./", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/bin/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/bin/hello", Mode: 0o755, Typeflag: archivetar.TypeReg, Size: 5, Uid: 1000, Gid: 1000, ModTime: modTime},
		{Name: "./usr/bin/link", Typeflag: archivetar.TypeSymlink, Linkname: "hello", ModTime: modTime},
		{Name: "./etc/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./etc/config", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 7, ModTime: modTime,
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.selinux": "system_u:object_r:usr_t:s0\x00",
			},
		},
	}

	fileContents := map[string][]byte{
		"./usr/bin/hello": []byte("hello"),
		"./etc/config":    []byte("foo=bar"),
	}

	// Step 1: overlay.Diff() generates canonical tar (simulates commit)
	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)
	for _, e := range entries {
		require.NoError(t, cw.WriteHeader(&e))
		if data, ok := fileContents[e.Name]; ok {
			_, err := cw.Write(data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, cw.Close())
	originalBytes := canonicalTar.Bytes()

	// Step 2: Compress with zstd:chunked (simulates Buildah compression)
	var compressed bytes.Buffer
	annotations := make(map[string]string)
	zstdWriter, err := compressor.ZstdCompressor(&compressed, annotations, nil)
	require.NoError(t, err)
	_, err = io.Copy(zstdWriter, bytes.NewReader(originalBytes))
	require.NoError(t, err)
	require.NoError(t, zstdWriter.Close())

	// Step 3: applyDiff decompresses and computes UncompressedDigest + tar-split
	// (simulates what layers.go:applyDiff does when storing the layer)
	uncompressedDigester := digest.Canonical.Digester()
	var tarSplitBuf bytes.Buffer
	tarSplitPacker := storage.NewJSONPacker(&tarSplitBuf)

	inputTarStream, done, err := asm.NewInputTarStreamWithDone(
		io.TeeReader(bytes.NewReader(originalBytes), uncompressedDigester.Hash()),
		tarSplitPacker,
		storage.NewDiscardFilePutter(),
	)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, inputTarStream)
	require.NoError(t, err)
	require.NoError(t, inputTarStream.Close())
	require.NoError(t, <-done)

	expectedDigest := uncompressedDigester.Digest()

	// Step 4: During push, Diff() reconstructs tar from tar-split
	// (simulates asm.NewOutputTarStream in layers.go:Diff())
	fg := &memFileGetter{files: fileContents}
	tarSplitUnpacker := storage.NewJSONUnpacker(&tarSplitBuf)
	var reconstructed bytes.Buffer
	err = asm.WriteOutputTarStream(fg, tarSplitUnpacker, &reconstructed)
	require.NoError(t, err)

	// Step 5: Verify digest matches
	actualDigest := digest.Canonical.FromBytes(reconstructed.Bytes())
	assert.Equal(t, expectedDigest, actualDigest,
		"reconstructed tar from tar-split should match UncompressedDigest computed during applyDiff")
	assert.Equal(t, originalBytes, reconstructed.Bytes(),
		"reconstructed tar should be byte-identical to original canonical tar")
}

func TestGenerateTarSplitEndToEnd(t *testing.T) {
	modTime := time.Unix(1700000000, 123456789)
	longDir := "./" + strings.Repeat("longsubdir/", 10)

	entries := []archivetar.Header{
		{Name: "./", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/bin/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./usr/bin/hello", Mode: 0o755, Typeflag: archivetar.TypeReg, Size: 5, Uid: 1000, Gid: 1000, ModTime: modTime},
		{Name: "./usr/bin/link", Typeflag: archivetar.TypeSymlink, Linkname: "hello", ModTime: modTime},
		{Name: "./usr/bin/hardlink", Typeflag: archivetar.TypeLink, Linkname: "./usr/bin/hello"},
		{Name: "./etc/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: "./etc/config", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 7, ModTime: modTime,
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.selinux": "system_u:object_r:usr_t:s0\x00",
			},
		},
		{Name: longDir, Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime},
		{Name: longDir + "file.txt", Mode: 0o644, Typeflag: archivetar.TypeReg, Size: 4, ModTime: modTime},
	}

	fileContents := map[string][]byte{
		"./usr/bin/hello":    []byte("hello"),
		"./etc/config":       []byte("foo=bar"),
		longDir + "file.txt": []byte("data"),
	}

	// Step 1: Build canonical tar
	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)
	for _, e := range entries {
		require.NoError(t, cw.WriteHeader(&e))
		if data, ok := fileContents[e.Name]; ok {
			_, err := cw.Write(data)
			require.NoError(t, err)
		}
	}
	require.NoError(t, cw.Close())
	originalBytes := canonicalTar.Bytes()

	// Step 2: Compress with zstd:chunked compressor (production path)
	var compressed bytes.Buffer
	annotations := make(map[string]string)
	zstdWriter, err := compressor.ZstdCompressor(&compressed, annotations, nil)
	require.NoError(t, err)
	_, err = io.Copy(zstdWriter, bytes.NewReader(originalBytes))
	require.NoError(t, err)
	require.NoError(t, zstdWriter.Close())

	// Step 3: Read back the TOC from the compressed blob (just like partial pull does)
	tocDigestStr, ok := annotations[minimal.ManifestChecksumKey]
	require.True(t, ok, "ManifestChecksumKey annotation missing")
	tocDigest, err := digest.Parse(tocDigestStr)
	require.NoError(t, err)

	compressedData := compressed.Bytes()
	seekable := &seekableBuffer{data: compressedData, t: t}
	_, parsedTOC, tarSplitFile, _, err := readZstdChunkedManifest(t.TempDir(), seekable, tocDigest, annotations, true)
	require.NoError(t, err)
	if tarSplitFile != nil {
		tarSplitFile.Close()
	}
	assert.True(t, parsedTOC.CanonicalTar, "TOC should have CanonicalTar=true")

	// Step 4: Generate tar-split from TOC
	fg := &memFileGetter{files: fileContents}
	tmpDir := t.TempDir()
	generatedTarSplit, err := generateTarSplitFromTOC(parsedTOC, fg, tmpDir)
	require.NoError(t, err)
	defer generatedTarSplit.Close()

	// Step 5: Reassemble tar from generated tar-split and compare
	metadata := storage.NewJSONUnpacker(generatedTarSplit)
	fg2 := &memFileGetter{files: fileContents}
	var reassembled bytes.Buffer
	err = asm.WriteOutputTarStream(fg2, metadata, &reassembled)
	require.NoError(t, err)

	assert.Equal(t, originalBytes, reassembled.Bytes(),
		"end-to-end: canonical tar → zstd:chunked compress → read TOC → generate tar-split → reassemble should produce identical tar")
}

// seekableBuffer implements ImageSourceSeekable for in-memory testing.
type seekableBuffer struct {
	data []byte
	t    *testing.T
}

func (s *seekableBuffer) GetBlobAt(req []ImageSourceChunk) (chan io.ReadCloser, chan error, error) {
	m := make(chan io.ReadCloser)
	e := make(chan error)
	go func() {
		for _, chunk := range req {
			end := chunk.Offset + chunk.Length
			if end > uint64(len(s.data)) {
				end = uint64(len(s.data))
			}
			m <- io.NopCloser(bytes.NewReader(s.data[chunk.Offset:end]))
		}
		close(m)
		close(e)
	}()
	return m, e, nil
}

// TestNonCanonicalTarRejected verifies that the compressor rejects
// non-canonical tar streams (missing global pax header with canonical-tar=1).
func TestNonCanonicalTarRejected(t *testing.T) {
	modTime := time.Unix(1700000000, 0)

	var standardTar bytes.Buffer
	tw := archivetar.NewWriter(&standardTar)
	require.NoError(t, tw.WriteHeader(&archivetar.Header{
		Name: "usr/", Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime,
	}))
	require.NoError(t, tw.WriteHeader(&archivetar.Header{
		Name: "usr/bin/hello", Mode: 0o755, Typeflag: archivetar.TypeReg, Size: 5, ModTime: modTime,
	}))
	_, err := tw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	var compressed bytes.Buffer
	annotations := make(map[string]string)
	zstdWriter, err := compressor.ZstdCompressor(&compressed, annotations, nil)
	require.NoError(t, err)
	_, err = io.Copy(zstdWriter, bytes.NewReader(standardTar.Bytes()))
	require.NoError(t, err)
	err = zstdWriter.Close()
	require.Error(t, err, "compressor must reject non-canonical tar")
	assert.Contains(t, err.Error(), "canonical tar")
}

func TestFullPullPushCycleWithHardlinks(t *testing.T) {
	modTime := time.Unix(1700000000, 0)

	fileContents := map[string][]byte{
		"./usr/bin/hello":       []byte("hello world binary content here"),
		"./usr/bin/large":       bytes.Repeat([]byte("ABCDEFGHIJ"), 10000),
		"./etc/config":          []byte("foo=bar"),
		"./etc/nginx/nginx.conf": []byte("worker_processes auto;"),
	}

	var canonicalTar bytes.Buffer
	cw := archive.NewCanonicalTarWriter(&canonicalTar)

	writeDir := func(name string) {
		require.NoError(t, cw.WriteHeader(&archivetar.Header{
			Name: name, Mode: 0o755, Typeflag: archivetar.TypeDir, ModTime: modTime,
		}))
	}
	writeFile := func(name string, content []byte, mode int64) {
		require.NoError(t, cw.WriteHeader(&archivetar.Header{
			Name: name, Mode: mode, Typeflag: archivetar.TypeReg, Size: int64(len(content)), ModTime: modTime,
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.selinux": "system_u:object_r:usr_t:s0\x00",
			},
		}))
		_, err := cw.Write(content)
		require.NoError(t, err)
	}

	writeDir("./usr/")
	writeDir("./usr/bin/")
	writeFile("./usr/bin/hello", fileContents["./usr/bin/hello"], 0o755)
	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name: "./usr/bin/hello.bak", Typeflag: archivetar.TypeLink, Linkname: "./usr/bin/hello",
	}))
	writeFile("./usr/bin/large", fileContents["./usr/bin/large"], 0o755)
	require.NoError(t, cw.WriteHeader(&archivetar.Header{
		Name: "./usr/bin/link", Typeflag: archivetar.TypeSymlink, Linkname: "hello", ModTime: modTime,
	}))
	writeDir("./etc/")
	writeFile("./etc/config", fileContents["./etc/config"], 0o644)
	writeDir("./etc/nginx/")
	writeFile("./etc/nginx/nginx.conf", fileContents["./etc/nginx/nginx.conf"], 0o644)
	require.NoError(t, cw.Close())

	// Compress with zstd:chunked
	var compressed bytes.Buffer
	annotations := make(map[string]string)
	zstdWriter, err := compressor.ZstdCompressor(&compressed, annotations, nil)
	require.NoError(t, err)
	_, err = io.Copy(zstdWriter, bytes.NewReader(canonicalTar.Bytes()))
	require.NoError(t, err)
	require.NoError(t, zstdWriter.Close())

	// Read TOC
	tocDigestStr := annotations[minimal.ManifestChecksumKey]
	tocDigest, err := digest.Parse(tocDigestStr)
	require.NoError(t, err)
	seekable := &seekableBuffer{data: compressed.Bytes(), t: t}
	_, parsedTOC, tarSplitFile, _, err := readZstdChunkedManifest(t.TempDir(), seekable, tocDigest, annotations, true)
	require.NoError(t, err)
	if tarSplitFile != nil {
		tarSplitFile.Close()
	}
	require.True(t, parsedTOC.CanonicalTar)

	// PULL: Generate tar-split from TOC, compute UncompressedDigest
	fg := &memFileGetter{files: fileContents}
	generatedTarSplit, err := generateTarSplitFromTOC(parsedTOC, fg, t.TempDir())
	require.NoError(t, err)
	defer generatedTarSplit.Close()

	metadata := storage.NewJSONUnpacker(generatedTarSplit)
	pullDigester := digest.Canonical.Digester()
	require.NoError(t, asm.WriteOutputTarStream(&memFileGetter{files: fileContents}, metadata, pullDigester.Hash()))
	pullDigest := pullDigester.Digest()

	// STORE: Write tar-split through gzip (like layers.go)
	_, err = generatedTarSplit.Seek(0, io.SeekStart)
	require.NoError(t, err)
	var gzipBuf bytes.Buffer
	gzWriter, err := pgzip.NewWriterLevel(&gzipBuf, pgzip.BestSpeed)
	require.NoError(t, err)
	_, err = io.Copy(gzWriter, generatedTarSplit)
	require.NoError(t, err)
	require.NoError(t, gzWriter.Close())

	// PUSH: Read tar-split from gzip, reconstruct via NewOutputTarStream (pipe-based, like Diff())
	gzReader, err := pgzip.NewReader(&gzipBuf)
	require.NoError(t, err)
	metadata2 := storage.NewJSONUnpacker(gzReader)
	tarStream := asm.NewOutputTarStream(&memFileGetter{files: fileContents}, metadata2)

	// Read entire stream (like GetBlob() → digestingReader does)
	pushDigester := digest.Canonical.Digester()
	_, err = io.Copy(pushDigester.Hash(), tarStream)
	require.NoError(t, err)
	require.NoError(t, tarStream.Close())
	require.NoError(t, gzReader.Close())
	pushDigest := pushDigester.Digest()

	assert.Equal(t, pullDigest, pushDigest,
		"push digest must match pull digest after full pull→store→push cycle with hardlinks")
}

func TestOpenTmpFile(t *testing.T) {
	tmpDir := t.TempDir()
	for range 1000 {
		// scope for cleanup
		f := func(fn func(tmpDir string) (*os.File, error)) {
			file, err := fn(tmpDir)
			assert.NoError(t, err)
			defer file.Close()

			path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
			assert.NoError(t, err)

			// the path under /proc/self/fd/$FD has the prefix "(deleted)" when the file
			// is unlinked
			assert.Contains(t, path, "(deleted)")
		}
		f(openTmpFile)
		f(openTmpFileNoTmpFile)
	}
}
