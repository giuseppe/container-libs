//go:build !windows

package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCanonicalTarSortedOrder(t *testing.T) {
	root := t.TempDir()

	// Create dirs and files in non-sorted order
	for _, d := range []string{"c", "a", "b", "a/z", "a/m"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	for _, f := range []string{"c/file", "a/z/file", "a/m/file", "b/file", "a/file"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, f), []byte("data"), 0o644))
	}

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, nil, nil)
	require.NoError(t, err)

	names := readTarNames(t, &buf)
	expected := []string{
		"./",
		"./a/",
		"./a/file",
		"./a/m/",
		"./a/m/file",
		"./a/z/",
		"./a/z/file",
		"./b/",
		"./b/file",
		"./c/",
		"./c/file",
	}
	assert.Equal(t, expected, names)
}

func TestCreateCanonicalTarHardlinks(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(root, "dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dir/original"), []byte("content"), 0o644))
	require.NoError(t, os.Link(filepath.Join(root, "dir/original"), filepath.Join(root, "dir/hardlink")))

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, nil, nil)
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	types := make(map[string]byte)
	links := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		types[hdr.Name] = hdr.Typeflag
		if hdr.Typeflag == tar.TypeLink {
			links[hdr.Name] = hdr.Linkname
		}
	}

	// "hardlink" comes before "original" lexicographically, so "hardlink" is the
	// first occurrence (regular file) and "original" becomes the link.
	assert.Equal(t, byte(tar.TypeReg), types["./dir/hardlink"])
	assert.Equal(t, byte(tar.TypeLink), types["./dir/original"])
	assert.Equal(t, "./dir/hardlink", links["./dir/original"])
}

func TestCreateCanonicalTarFilterSkip(t *testing.T) {
	root := t.TempDir()

	for _, d := range []string{"include", "exclude", "exclude/sub"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "include/file"), []byte("yes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "exclude/file"), []byte("no"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "exclude/sub/file"), []byte("no"), 0o644))

	filter := func(path string, hdr *tar.Header) bool {
		return path != "exclude"
	}

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, filter, nil)
	require.NoError(t, err)

	names := readTarNames(t, &buf)
	for _, name := range names {
		assert.NotContains(t, name, "exclude", "excluded directory and contents should be skipped")
	}
	assert.Contains(t, names, "./include/")
	assert.Contains(t, names, "./include/file")
}

func TestCreateCanonicalTarFilterModify(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0o644))

	filter := func(path string, hdr *tar.Header) bool {
		hdr.Uid = 1000
		hdr.Gid = 1000
		return true
	}

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, filter, nil)
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		assert.Equal(t, 1000, hdr.Uid)
		assert.Equal(t, 1000, hdr.Gid)
	}
}

func TestCreateCanonicalTarSymlinks(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(root, "target"), []byte("data"), 0o644))
	require.NoError(t, os.Symlink("target", filepath.Join(root, "link")))

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, nil, nil)
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Name == "./link" {
			assert.Equal(t, byte(tar.TypeSymlink), hdr.Typeflag)
			assert.Equal(t, "target", hdr.Linkname)
		}
	}
}

func TestCreateCanonicalTarDeterministic(t *testing.T) {
	root := t.TempDir()

	for _, d := range []string{"x", "y"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	for _, f := range []string{"x/a", "x/b", "y/c"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, f), []byte(f), 0o644))
	}

	var buf1, buf2 bytes.Buffer
	require.NoError(t, CreateCanonicalTar(root, &buf1, nil, nil))
	require.NoError(t, CreateCanonicalTar(root, &buf2, nil, nil))

	assert.Equal(t, buf1.Bytes(), buf2.Bytes(), "two runs on the same tree must produce identical output")
}

func TestCreateCanonicalTarGlobalHeader(t *testing.T) {
	root := t.TempDir()

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, nil, nil)
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.Equal(t, byte(tar.TypeXGlobalHeader), hdr.Typeflag)
	assert.Equal(t, "1", hdr.PAXRecords["canonical-tar"])
}

func TestCreateCanonicalTarExtraContent(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("rootfs"), 0o644))

	extraFile := filepath.Join(t.TempDir(), "sbom.json")
	require.NoError(t, os.WriteFile(extraFile, []byte(`{"sbom": true}`), 0o644))

	extra := map[string]string{
		"/sbom.json": extraFile,
	}

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, nil, extra)
	require.NoError(t, err)

	names := readTarNames(t, &buf)
	assert.Contains(t, names, "./file")
	assert.Contains(t, names, "./sbom.json")
}

func TestCreateCanonicalTarExtraContentFilter(t *testing.T) {
	root := t.TempDir()

	extraFile := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.WriteFile(extraFile, []byte("content"), 0o644))

	filter := func(path string, hdr *tar.Header) bool {
		hdr.Uid = 42
		hdr.Gid = 42
		return true
	}

	extra := map[string]string{
		"/extra": extraFile,
	}

	var buf bytes.Buffer
	err := CreateCanonicalTar(root, &buf, filter, extra)
	require.NoError(t, err)

	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		assert.Equal(t, 42, hdr.Uid, "filter should apply to entry %q", hdr.Name)
		assert.Equal(t, 42, hdr.Gid, "filter should apply to entry %q", hdr.Name)
	}
}

func readTarNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		names = append(names, hdr.Name)
	}
	return names
}
