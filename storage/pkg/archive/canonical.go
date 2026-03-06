package archive

import (
	"archive/tar"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	// PaxCanonical is the PAX record key that marks a tar entry as canonical.
	PaxCanonical = "CONTAINERS.canonical"
)

// CanonicalizeTarHeader mutates hdr in place to conform to the canonical tar format:
//  1. PAX format only
//  2. CONTAINERS.canonical=1 PAX record
//  3. Uname/Gname = ""
//  4. Mode &= 0o7777 (strip file type bits)
//  5. Paths cleaned: strip ./ prefix, directories get trailing /
//  6. AccessTime/ChangeTime zeroed
//  7. Only SCHILY.xattr.* PAX records preserved
//  8. Hardlinks: Size = 0
//  9. ModTime preserved with nanosecond precision via PAX mtime
func CanonicalizeTarHeader(hdr *tar.Header) {
	hdr.Format = tar.FormatPAX

	// Clean the path
	hdr.Name = canonicalPath(hdr.Name, hdr.Typeflag == tar.TypeDir)

	if hdr.Typeflag == tar.TypeLink {
		hdr.Linkname = canonicalPath(hdr.Linkname, false)
		hdr.Size = 0
	}

	hdr.Uname = ""
	hdr.Gname = ""
	hdr.Mode &= 0o7777

	hdr.AccessTime = time.Time{}
	hdr.ChangeTime = time.Time{}

	// Filter PAX records: only keep SCHILY.xattr.* and add CONTAINERS.canonical=1
	filtered := make(map[string]string)
	for k, v := range hdr.PAXRecords {
		if strings.HasPrefix(k, PaxSchilyXattr) {
			filtered[k] = v
		}
	}
	filtered[PaxCanonical] = "1"
	hdr.PAXRecords = filtered
}

// IsCanonicalHeader checks if a tar header conforms to canonical format.
// Returns nil if canonical, or an error describing the first non-canonical aspect.
func IsCanonicalHeader(hdr *tar.Header) error {
	if hdr.Format != tar.FormatPAX && hdr.Format != tar.FormatUnknown {
		return fmt.Errorf("non-PAX format: %v", hdr.Format)
	}
	if hdr.PAXRecords[PaxCanonical] != "1" {
		return fmt.Errorf("missing %s=1 PAX record", PaxCanonical)
	}
	if hdr.Uname != "" {
		return fmt.Errorf("non-empty Uname: %q", hdr.Uname)
	}
	if hdr.Gname != "" {
		return fmt.Errorf("non-empty Gname: %q", hdr.Gname)
	}
	if hdr.Mode != hdr.Mode&0o7777 {
		return fmt.Errorf("mode has file type bits: %o", hdr.Mode)
	}
	if !hdr.AccessTime.IsZero() {
		return fmt.Errorf("non-zero AccessTime")
	}
	if !hdr.ChangeTime.IsZero() {
		return fmt.Errorf("non-zero ChangeTime")
	}
	if hdr.Typeflag == tar.TypeLink && hdr.Size != 0 {
		return fmt.Errorf("hardlink with non-zero size: %d", hdr.Size)
	}

	expectedName := canonicalPath(hdr.Name, hdr.Typeflag == tar.TypeDir)
	if hdr.Name != expectedName {
		return fmt.Errorf("non-canonical path %q (expected %q)", hdr.Name, expectedName)
	}

	for k := range hdr.PAXRecords {
		if k == PaxCanonical {
			continue
		}
		if !strings.HasPrefix(k, PaxSchilyXattr) {
			return fmt.Errorf("non-xattr PAX record: %q", k)
		}
	}

	return nil
}

// ComparePathComponents compares two paths component by component,
// matching the order produced by filepath.WalkDir (depth-first lexical).
// Directories sort before their contents because they have a trailing /.
func ComparePathComponents(a, b string) int {
	aParts := strings.Split(a, "/")
	bParts := strings.Split(b, "/")

	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] != bParts[i] {
			// If one of them is the last component and empty (trailing /),
			// the directory (shorter path) comes first.
			if aParts[i] < bParts[i] {
				return -1
			}
			return 1
		}
	}

	if len(aParts) < len(bParts) {
		return -1
	}
	if len(aParts) > len(bParts) {
		return 1
	}
	return 0
}

// canonicalPath cleans a path for canonical tar format.
func canonicalPath(name string, isDir bool) string {
	// Strip ./ prefix
	name = strings.TrimPrefix(name, "./")
	if name == "." || name == "" {
		if isDir {
			return "./"
		}
		return name
	}

	// Clean the path
	name = path.Clean(name)

	// Directories get trailing /
	if isDir && !strings.HasSuffix(name, "/") {
		name += "/"
	}

	return name
}
