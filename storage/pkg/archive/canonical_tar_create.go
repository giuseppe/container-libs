//go:build !windows

package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/pkg/pools"
	"golang.org/x/sys/unix"
)

// CreateCanonicalTar walks root in canonical order (depth-first, lexicographic
// per directory) and writes a canonical tar to w. The walk uses fd-based
// traversal (openat with O_NOFOLLOW) to prevent symlink-based TOCTOU attacks.
//
// For each entry, filter is called with the relative path and a mutable header.
// Return false to skip the entry (and its contents if it's a directory).
// Return true to include it, after optionally modifying the header in place.
//
// extra maps destination archive paths to source host paths for additional
// regular files to include after the directory walk. These entries are written
// with mode 0644 and uid/gid 0:0. The filter applies to them as well.
// A nil or empty extra map adds no extra entries.
//
// A nil filter includes everything unchanged.
func CreateCanonicalTar(root string, w io.Writer, filter func(path string, hdr *tar.Header) bool, extra map[string]string) error {
	ta := newTarWriter(&idtools.IDMappings{}, w, nil, nil)
	defer pools.BufioWriter32KPool.Put(ta.Buffer)

	rootFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening root %q: %w", root, err)
	}
	defer unix.Close(rootFd)

	rootPath := fmt.Sprintf("/proc/self/fd/%d", rootFd)
	data, err := ta.prepareAddFile(rootPath, ".")
	if err != nil {
		ta.TarWriter.Close()
		return err
	}
	if data != nil {
		if filter == nil || filter(".", data.hdr) {
			if err := ta.addFile(data); err != nil {
				ta.TarWriter.Close()
				return err
			}
		}
	}

	if err := walkCanonical(rootFd, ".", ta, filter); err != nil {
		ta.TarWriter.Close()
		return err
	}

	if err := writeExtraContent(ta, filter, extra); err != nil {
		ta.TarWriter.Close()
		return err
	}

	return ta.TarWriter.Close()
}

// writeExtraContent writes additional file entries to the tar stream.
// Entries are written in sorted order with mode 0644 and uid/gid 0:0.
func writeExtraContent(ta *tarWriter, filter func(string, *tar.Header) bool, extra map[string]string) error {
	if len(extra) == 0 {
		return nil
	}

	destPaths := slices.Sorted(func(yield func(string) bool) {
		for k := range extra {
			if !yield(k) {
				return
			}
		}
	})

	for _, destPath := range destPaths {
		srcPath := extra[destPath]

		st, err := os.Stat(srcPath)
		if err != nil {
			return fmt.Errorf("stat extra content %q: %w", srcPath, err)
		}

		name := destPath
		if !strings.HasPrefix(name, "./") {
			name = "./" + strings.TrimPrefix(name, "/")
		}

		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     st.Size(),
			Mode:     0o644,
			ModTime:  st.ModTime(),
		}

		if filter != nil && !filter(destPath, hdr) {
			continue
		}

		if err := ta.TarWriter.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing header for extra content %q: %w", destPath, err)
		}

		if hdr.Size > 0 {
			f, err := os.Open(srcPath)
			if err != nil {
				return fmt.Errorf("opening extra content %q: %w", srcPath, err)
			}
			_, err = io.Copy(ta.TarWriter, f)
			f.Close()
			if err != nil {
				return fmt.Errorf("writing extra content %q: %w", destPath, err)
			}
		}
	}
	return nil
}

// walkCanonical enumerates the directory referenced by parentFd and
// recurses into subdirectories. All child opens use openat(2) with
// O_NOFOLLOW so symlinks are never followed, eliminating TOCTOU races
// where a directory could be replaced with a symlink between stat and open.
func walkCanonical(parentFd int, relDir string, ta *tarWriter, filter func(string, *tar.Header) bool) error {
	dirFile := os.NewFile(uintptr(parentFd), relDir)
	entries, err := dirFile.ReadDir(-1)
	// Do NOT close dirFile — the caller owns parentFd.
	if err != nil {
		return fmt.Errorf("reading directory %q: %w", relDir, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	parentPath := fmt.Sprintf("/proc/self/fd/%d", parentFd)

	for _, entry := range entries {
		name := entry.Name()
		relPath := filepath.Join(relDir, name)
		safePath := filepath.Join(parentPath, name)

		data, err := ta.prepareAddFile(safePath, relPath)
		if err != nil {
			return err
		}
		if data == nil {
			continue
		}
		if filter != nil && !filter(relPath, data.hdr) {
			continue
		}
		if err := ta.addFile(data); err != nil {
			return err
		}

		if data.fi.IsDir() {
			childFd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("openat %q in %q: %w", name, relDir, err)
			}
			err = walkCanonical(childFd, relPath, ta, filter)
			unix.Close(childFd)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
