package archive

import (
	"archive/tar"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

const canonicalPAXRecordKey = "CONTAINERS.canonical"

// canonicalizePath normalizes a tar entry path:
//   - strips "./" prefix
//   - cleans the path (removes double slashes, etc.)
//   - ensures directories have a trailing "/"
func canonicalizePath(name string, isDir bool) string {
	name = strings.TrimPrefix(name, "./")
	cleaned := path.Clean(name)
	if cleaned == "." {
		cleaned = ""
	}
	if isDir && cleaned != "" && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// CanonicalizeTarHeader normalizes a tar header so that the same metadata
// always produces the same binary tar output. This is the foundation for
// eliminating tarsplit from zstd:chunked images.
//
// Rules:
//   - Always PAX format
//   - Path normalized (no "./" prefix, directories have trailing "/")
//   - Uname/Gname always empty (irrelevant for containers)
//   - Mode stripped to permission + setuid/setgid/sticky bits (no file type bits)
//   - AccessTime/ChangeTime zeroed (ctime is kernel-managed and cannot be
//     preserved across extract/re-tar cycles; atime is also unreliable)
//   - Only SCHILY.xattr.* PAX records are preserved
//   - A non-basic PAX record is added to force PAX format
//   - Hardlink size is set to 0
func CanonicalizeTarHeader(hdr *tar.Header) {
	hdr.Format = tar.FormatPAX
	hdr.Uname = ""
	hdr.Gname = ""

	isDir := hdr.Typeflag == tar.TypeDir
	hdr.Name = canonicalizePath(hdr.Name, isDir)
	if hdr.Linkname != "" {
		hdr.Linkname = canonicalizePath(hdr.Linkname, false)
	}

	hdr.Mode &= 0o7777

	hdr.AccessTime = time.Time{}
	hdr.ChangeTime = time.Time{}

	if hdr.PAXRecords == nil {
		hdr.PAXRecords = make(map[string]string)
	}

	for k := range hdr.PAXRecords {
		if !strings.HasPrefix(k, PaxSchilyXattr) {
			delete(hdr.PAXRecords, k)
		}
	}

	hdr.PAXRecords[canonicalPAXRecordKey] = "1"

	if hdr.Typeflag == tar.TypeLink {
		hdr.Size = 0
	}
}

// CanonicalTOCBigDataKey is the BigData key used to store the canonical
// TOC for v2 zstd:chunked layers. The TOC is saved during partial pull
// so that Diff() can reconstruct a tar stream in the same entry order.
const CanonicalTOCBigDataKey = "canonical-toc"

// CanonicalTOC is a minimal representation of the zstd:chunked TOC,
// used for canonical tar reconstruction. It mirrors the essential fields
// of the internal minimal.TOC type.
type CanonicalTOC struct {
	Version int                `json:"version"`
	Entries []CanonicalTOCEntry `json:"entries"`
}

// CanonicalTOCEntry mirrors the essential fields of minimal.FileMetadata
// needed to reconstruct canonical tar headers.
type CanonicalTOCEntry struct {
	Type     string            `json:"type"`
	Name     string            `json:"name"`
	Linkname string            `json:"linkName,omitempty"`
	Mode     int64             `json:"mode,omitempty"`
	Size     int64             `json:"size,omitempty"`
	UID      int               `json:"uid,omitempty"`
	GID      int               `json:"gid,omitempty"`
	ModTime  *time.Time        `json:"modtime,omitempty"`
	Devmajor int64             `json:"devMajor,omitempty"`
	Devminor int64             `json:"devMinor,omitempty"`
	Xattrs   map[string]string `json:"xattrs,omitempty"`
}

// tocTypeToTarType maps TOC type strings to tar typeflag bytes.
var tocTypeToTarType = map[string]byte{
	"reg":      tar.TypeReg,
	"hardlink": tar.TypeLink,
	"char":     tar.TypeChar,
	"block":    tar.TypeBlock,
	"dir":      tar.TypeDir,
	"fifo":     tar.TypeFifo,
	"symlink":  tar.TypeSymlink,
}

// FileGetter provides file contents by name for canonical tar reconstruction.
type FileGetter interface {
	Get(filename string) (io.ReadCloser, error)
}

// tocEntryToCanonicalTarHeader converts a CanonicalTOCEntry to a
// canonical tar.Header. This mirrors minimal.FileMetadataToCanonicalTarHeader.
func tocEntryToCanonicalTarHeader(e *CanonicalTOCEntry) (*tar.Header, error) {
	typeflag, ok := tocTypeToTarType[e.Type]
	if !ok {
		return nil, fmt.Errorf("unknown type %q for %q", e.Type, e.Name)
	}

	hdr := &tar.Header{
		Typeflag: typeflag,
		Name:     e.Name,
		Linkname: e.Linkname,
		Size:     e.Size,
		Mode:     e.Mode,
		Uid:      e.UID,
		Gid:      e.GID,
		Devmajor: e.Devmajor,
		Devminor: e.Devminor,
	}

	if e.ModTime != nil {
		hdr.ModTime = *e.ModTime
	}

	hdr.PAXRecords = make(map[string]string)
	for k, v := range e.Xattrs {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("decoding xattr %q for %q: %w", k, e.Name, err)
		}
		hdr.PAXRecords[PaxSchilyXattr+k] = string(decoded)
	}

	CanonicalizeTarHeader(hdr)
	return hdr, nil
}

// WriteCanonicalTar reads a TOC from tocReader (JSON) and writes a canonical
// tar stream to w, fetching file contents from fg. The tar entries are written
// in the order they appear in the TOC, ensuring the output matches the digest
// computed during partial pull.
func WriteCanonicalTar(tocReader io.Reader, fg FileGetter, w io.Writer) error {
	var toc CanonicalTOC
	if err := json.NewDecoder(tocReader).Decode(&toc); err != nil {
		return fmt.Errorf("parsing canonical TOC: %w", err)
	}

	tw := tar.NewWriter(w)

	for i := range toc.Entries {
		e := &toc.Entries[i]

		// TypeChunk entries are sub-file chunks; they don't correspond to
		// separate tar entries.
		if e.Type == "chunk" {
			continue
		}

		hdr, err := tocEntryToCanonicalTarHeader(e)
		if err != nil {
			return err
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing tar header for %q: %w", e.Name, err)
		}

		if hdr.Size > 0 {
			rc, err := fg.Get(e.Name)
			if err != nil {
				return fmt.Errorf("getting file %q: %w", e.Name, err)
			}
			if _, err := io.Copy(tw, rc); err != nil {
				rc.Close()
				return fmt.Errorf("writing file content for %q: %w", e.Name, err)
			}
			rc.Close()
		}
	}

	return tw.Close()
}

// NewCanonicalTarFilter wraps a tar stream so that every header is
// canonicalized before it is re-emitted. File content passes through
// unchanged.
//
// This is used when Diff() produces a tar from the filesystem for
// layers whose UncompressedDigest was computed from a canonical tar
// (v2 zstd:chunked). The filter ensures the output matches the
// digest stored during partial pull.
func NewCanonicalTarFilter(src io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()

	go func() {
		err := func() error {
			tr := tar.NewReader(src)
			tw := tar.NewWriter(pw)

			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("reading tar entry: %w", err)
				}

				CanonicalizeTarHeader(hdr)

				if err := tw.WriteHeader(hdr); err != nil {
					return fmt.Errorf("writing canonical header for %q: %w", hdr.Name, err)
				}

				if hdr.Size > 0 {
					if _, err := io.Copy(tw, tr); err != nil {
						return fmt.Errorf("copying content for %q: %w", hdr.Name, err)
					}
				}
			}

			return tw.Close()
		}()

		src.Close()
		pw.CloseWithError(err) //nolint:errcheck
	}()

	return pr
}
