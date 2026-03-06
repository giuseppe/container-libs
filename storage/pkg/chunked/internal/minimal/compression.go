package minimal

// NOTE: This is used from github.com/containers/image by callers that
// don't otherwise use containers/storage, so don't make this depend on any
// larger software like the graph drivers.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/vbatts/tar-split/archive/tar"
	"go.podman.io/storage/pkg/archive"
)

// ZstdWriter is an interface that wraps standard io.WriteCloser and Reset() to reuse the compressor with a new writer.
type ZstdWriter interface {
	io.WriteCloser
	Reset(dest io.Writer)
}

// CreateZstdWriterFunc is a function that creates a ZstdWriter for the provided destination writer.
type CreateZstdWriterFunc func(dest io.Writer) (ZstdWriter, error)

// TOC is short for Table of Contents and is used by the zstd:chunked
// file format to effectively add an overall index into the contents
// of a tarball; it also includes file metadata.
type TOC struct {
	// Version is 1 for traditional zstd:chunked (with tarsplit) and 2 for
	// canonical tar format (without tarsplit).
	Version int `json:"version"`
	// Entries is the list of file metadata in this TOC.
	// The ordering in this array currently defaults to being the same
	// as that of the tar stream; however, this should not be relied on.
	Entries []FileMetadata `json:"entries"`
	// TarSplitDigest is the checksum of the "tar-split" data which
	// is included as a distinct skippable zstd frame before the TOC.
	TarSplitDigest digest.Digest `json:"tarSplitDigest,omitempty"`
}

// FileMetadata is an entry in the TOC that includes both generic file metadata
// that duplicates what can found in the tar header (and should match), but
// also special/custom content (see below).
//
// Regular files may optionally be represented as a sequence of “chunks”,
// which may be ChunkTypeData or ChunkTypeZeros (and ChunkTypeData boundaries
// are heuristically determined to increase chance of chunk matching / reuse
// similar to rsync). In that case, the regular file is represented
// as an initial TypeReg entry (with all metadata for the file as a whole)
// immediately followed by zero or more TypeChunk entries (containing only Type,
// Name and Chunk* fields); if there is at least one TypeChunk entry, the Chunk*
// fields are relevant in all of these entries, including the initial
// TypeReg one.
//
// Note that the metadata here, when fetched by a zstd:chunked aware client,
// is used instead of that in the tar stream.  The contents of the tar stream
// are not used in this scenario.
type FileMetadata struct {
	// If you add any fields, update ensureFileMetadataMatches as well!

	// The metadata below largely duplicates that in the tar headers.
	Type       string            `json:"type"`
	Name       string            `json:"name"`
	Linkname   string            `json:"linkName,omitempty"`
	Mode       int64             `json:"mode,omitempty"`
	Size       int64             `json:"size,omitempty"`
	UID        int               `json:"uid,omitempty"`
	GID        int               `json:"gid,omitempty"`
	ModTime    *time.Time        `json:"modtime,omitempty"`
	AccessTime *time.Time        `json:"accesstime,omitempty"`
	ChangeTime *time.Time        `json:"changetime,omitempty"`
	Devmajor   int64             `json:"devMajor,omitempty"`
	Devminor   int64             `json:"devMinor,omitempty"`
	Xattrs     map[string]string `json:"xattrs,omitempty"`
	// Digest is a hexadecimal sha256 checksum of the file contents; it
	// is empty for empty files
	Digest    string `json:"digest,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	EndOffset int64  `json:"endOffset,omitempty"`

	ChunkSize   int64  `json:"chunkSize,omitempty"`
	ChunkOffset int64  `json:"chunkOffset,omitempty"`
	ChunkDigest string `json:"chunkDigest,omitempty"`
	ChunkType   string `json:"chunkType,omitempty"`
}

const (
	ChunkTypeData  = ""
	ChunkTypeZeros = "zeros"
)

const (
	// The following types correspond to regular types of entries that can
	// appear in a tar archive.
	TypeReg     = "reg"
	TypeLink    = "hardlink"
	TypeChar    = "char"
	TypeBlock   = "block"
	TypeDir     = "dir"
	TypeFifo    = "fifo"
	TypeSymlink = "symlink"
	// TypeChunk is special; in zstd:chunked not only are files individually
	// compressed and indexable, there is a "rolling checksum" used to compute
	// "chunks" of individual file contents, that are also added to the TOC
	TypeChunk = "chunk"
)

var TarTypes = map[byte]string{
	tar.TypeReg:     TypeReg,
	tar.TypeLink:    TypeLink,
	tar.TypeChar:    TypeChar,
	tar.TypeBlock:   TypeBlock,
	tar.TypeDir:     TypeDir,
	tar.TypeFifo:    TypeFifo,
	tar.TypeSymlink: TypeSymlink,
}

func GetType(t byte) (string, error) {
	r, found := TarTypes[t]
	if !found {
		return "", fmt.Errorf("unknown tarball type: %v", t)
	}
	return r, nil
}

// TypeToTarType converts a TOC type string back to a tar typeflag byte.
func TypeToTarType(t string) (byte, error) {
	for k, v := range TarTypes {
		if v == t {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown type: %v", t)
}

// FileMetadataToCanonicalTarHeader converts a FileMetadata entry to a
// canonical tar.Header suitable for writing a reproducible tar stream.
func FileMetadataToCanonicalTarHeader(e *FileMetadata) (*tar.Header, error) {
	typeflag, err := TypeToTarType(e.Type)
	if err != nil {
		return nil, fmt.Errorf("converting type for %q: %w", e.Name, err)
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
	// AccessTime and ChangeTime are intentionally not set;
	// CanonicalizeTarHeader zeros them because they can't be reliably
	// preserved across extract/re-tar cycles.

	hdr.PAXRecords = make(map[string]string)
	for k, v := range e.Xattrs {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("decoding xattr %q for %q: %w", k, e.Name, err)
		}
		hdr.PAXRecords[archive.PaxSchilyXattr+k] = string(decoded)
	}

	CanonicalizeTarHeader(hdr)
	return hdr, nil
}

// WriteCanonicalTar writes a canonical tar stream from TOC entries and a
// FileGetter that provides file contents. It produces deterministic output
// suitable for computing DiffIDs without tarsplit data.
func WriteCanonicalTar(toc *TOC, fg FileGetter, w io.Writer) error {
	tw := tar.NewWriter(w)

	for i := range toc.Entries {
		e := &toc.Entries[i]

		// TypeChunk entries are sub-file chunks; they don't correspond to
		// separate tar entries.
		if e.Type == TypeChunk {
			continue
		}

		hdr, err := FileMetadataToCanonicalTarHeader(e)
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

// FileGetter is an interface for retrieving file contents by name.
type FileGetter interface {
	Get(filename string) (io.ReadCloser, error)
}

const (
	// ManifestChecksumKey is a hexadecimal sha256 digest of the compressed manifest digest.
	ManifestChecksumKey = "io.github.containers.zstd-chunked.manifest-checksum"
	// ManifestInfoKey is an annotation that signals the start of the TOC (manifest)
	// contents which are embedded as a skippable zstd frame.  It has a format of
	// four decimal integers separated by `:` as follows:
	// <offset>:<length>:<uncompressed length>:<type>
	// The <type> is ManifestTypeCRFS which should have the value `1`.
	ManifestInfoKey = "io.github.containers.zstd-chunked.manifest-position"
	// TarSplitInfoKey is an annotation that signals the start of the "tar-split" metadata
	// contents which are embedded as a skippable zstd frame.  It has a format of
	// three decimal integers separated by `:` as follows:
	// <offset>:<length>:<uncompressed length>
	TarSplitInfoKey = "io.github.containers.zstd-chunked.tarsplit-position"

	// TarSplitChecksumKey is no longer used and is replaced by the TOC.TarSplitDigest field instead.
	// The value is retained here as a constant as a historical reference for older zstd:chunked images.
	//
	// Deprecated: This field should never be relied on - use the digest in the TOC instead.
	TarSplitChecksumKey = "io.github.containers.zstd-chunked.tarsplit-checksum"

	// UncompressedDigestKey is an annotation containing the digest of the
	// canonical uncompressed tar stream produced by the compressor.
	// For v2 zstd:chunked images, the compressor canonicalizes tar headers,
	// so the uncompressed content inside the blob differs from the original
	// input. This annotation allows the caller (e.g. c/image) to use the
	// correct DiffID for the image config.
	UncompressedDigestKey = "io.github.containers.zstd-chunked.uncompressed-digest"

	// ManifestTypeCRFS is a manifest file compatible with the CRFS TOC file.
	ManifestTypeCRFS = 1

	// FooterSizeSupported is the footer size supported by this implementation.
	// Newer versions of the image format might increase this value, so reject
	// any version that is not supported.
	FooterSizeSupported = 64
)

var (
	// when the zstd decoder encounters a skippable frame + 1 byte for the size, it
	// will ignore it.
	// https://tools.ietf.org/html/rfc8478#section-3.1.2
	skippableFrameMagic = []byte{0x50, 0x2a, 0x4d, 0x18}

	ZstdChunkedFrameMagic = []byte{0x47, 0x4e, 0x55, 0x6c, 0x49, 0x6e, 0x55, 0x78}
)

func appendZstdSkippableFrame(dest io.Writer, data []byte) error {
	if _, err := dest.Write(skippableFrameMagic); err != nil {
		return err
	}

	size := make([]byte, 4)
	binary.LittleEndian.PutUint32(size, uint32(len(data)))
	if _, err := dest.Write(size); err != nil {
		return err
	}
	if _, err := dest.Write(data); err != nil {
		return err
	}
	return nil
}

type TarSplitData struct {
	Data             []byte
	Digest           digest.Digest
	UncompressedSize int64
}

func WriteZstdChunkedManifest(dest io.Writer, outMetadata map[string]string, offset uint64, tarSplitData *TarSplitData, metadata []FileMetadata, createZstdWriter CreateZstdWriterFunc) error {
	// 8 is the size of the zstd skippable frame header + the frame size
	const zstdSkippableFrameHeader = 8
	manifestOffset := offset + zstdSkippableFrameHeader

	tocVersion := 1
	var tarSplitDigest digest.Digest
	if tarSplitData != nil {
		tarSplitDigest = tarSplitData.Digest
	} else {
		tocVersion = 2
	}

	toc := TOC{
		Version:        tocVersion,
		Entries:        metadata,
		TarSplitDigest: tarSplitDigest,
	}

	json := jsoniter.ConfigCompatibleWithStandardLibrary
	// Generate the manifest
	manifest, err := json.Marshal(toc)
	if err != nil {
		return err
	}

	var compressedBuffer bytes.Buffer
	zstdWriter, err := createZstdWriter(&compressedBuffer)
	if err != nil {
		return err
	}
	if _, err := zstdWriter.Write(manifest); err != nil {
		zstdWriter.Close()
		return err
	}
	if err := zstdWriter.Close(); err != nil {
		return err
	}
	compressedManifest := compressedBuffer.Bytes()

	manifestDigester := digest.Canonical.Digester()
	manifestChecksum := manifestDigester.Hash()
	if _, err := manifestChecksum.Write(compressedManifest); err != nil {
		return err
	}

	outMetadata[ManifestChecksumKey] = manifestDigester.Digest().String()
	outMetadata[ManifestInfoKey] = fmt.Sprintf("%d:%d:%d:%d", manifestOffset, len(compressedManifest), len(manifest), ManifestTypeCRFS)
	if err := appendZstdSkippableFrame(dest, compressedManifest); err != nil {
		return err
	}

	var tarSplitOffset uint64
	var tarSplitCompressedLen int
	var tarSplitUncompressedSize int64
	if tarSplitData != nil {
		tarSplitOffset = manifestOffset + uint64(len(compressedManifest)) + zstdSkippableFrameHeader
		tarSplitCompressedLen = len(tarSplitData.Data)
		tarSplitUncompressedSize = tarSplitData.UncompressedSize
		outMetadata[TarSplitInfoKey] = fmt.Sprintf("%d:%d:%d", tarSplitOffset, tarSplitCompressedLen, tarSplitUncompressedSize)
		if err := appendZstdSkippableFrame(dest, tarSplitData.Data); err != nil {
			return err
		}
	}

	footer := ZstdChunkedFooterData{
		ManifestType:               uint64(ManifestTypeCRFS),
		Offset:                     manifestOffset,
		LengthCompressed:           uint64(len(compressedManifest)),
		LengthUncompressed:         uint64(len(manifest)),
		OffsetTarSplit:             tarSplitOffset,
		LengthCompressedTarSplit:   uint64(tarSplitCompressedLen),
		LengthUncompressedTarSplit: uint64(tarSplitUncompressedSize),
	}

	manifestDataLE := footerDataToBlob(footer)

	return appendZstdSkippableFrame(dest, manifestDataLE)
}

func ZstdWriterWithLevel(dest io.Writer, level int) (ZstdWriter, error) {
	el := zstd.EncoderLevelFromZstd(level)
	return zstd.NewWriter(dest, zstd.WithEncoderLevel(el))
}

// ZstdChunkedFooterData contains all the data stored in the zstd:chunked footer.
// This footer exists to make the blobs self-describing, our implementation
// never reads it:
// Partial pull security hinges on the TOC digest, and that exists as a layer annotation;
// so we are relying on the layer annotations anyway, and doing so means we can avoid
// a round-trip to fetch this binary footer.
type ZstdChunkedFooterData struct {
	ManifestType uint64

	Offset             uint64
	LengthCompressed   uint64
	LengthUncompressed uint64

	OffsetTarSplit             uint64
	LengthCompressedTarSplit   uint64
	LengthUncompressedTarSplit uint64
	ChecksumAnnotationTarSplit string // Deprecated: This field is not a part of the footer and not used for any purpose.
}

func footerDataToBlob(footer ZstdChunkedFooterData) []byte {
	// Store the offset to the manifest and its size in LE order
	manifestDataLE := make([]byte, FooterSizeSupported)
	binary.LittleEndian.PutUint64(manifestDataLE[8*0:], footer.Offset)
	binary.LittleEndian.PutUint64(manifestDataLE[8*1:], footer.LengthCompressed)
	binary.LittleEndian.PutUint64(manifestDataLE[8*2:], footer.LengthUncompressed)
	binary.LittleEndian.PutUint64(manifestDataLE[8*3:], footer.ManifestType)
	binary.LittleEndian.PutUint64(manifestDataLE[8*4:], footer.OffsetTarSplit)
	binary.LittleEndian.PutUint64(manifestDataLE[8*5:], footer.LengthCompressedTarSplit)
	binary.LittleEndian.PutUint64(manifestDataLE[8*6:], footer.LengthUncompressedTarSplit)
	copy(manifestDataLE[8*7:], ZstdChunkedFrameMagic)

	return manifestDataLE
}

// timeIfNotZero returns a pointer to the time.Time if it is not zero, otherwise it returns nil.
func timeIfNotZero(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	return t
}

const canonicalPAXRecordKey = "CONTAINERS.canonical"

// canonicalizePath normalizes a tar entry path:
//   - strips "./" prefix
//   - cleans the path (removes double slashes, etc.)
//   - ensures directories have a trailing "/"
func canonicalizePath(name string, isDir bool) string {
	// Strip leading "./" prefix.
	name = strings.TrimPrefix(name, "./")
	// Clean the path but preserve trailing slash for directories.
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
//   - AccessTime/ChangeTime zeroed (ctime is kernel-managed and cannot be
//     preserved across extract/re-tar cycles; atime is also unreliable)
//   - Only SCHILY.xattr.* PAX records are preserved
//   - A non-basic PAX record is added to force PAX format
//   - Mode stripped to permission + setuid/setgid/sticky bits (no file type bits)
//   - Hardlink size is set to 0
func CanonicalizeTarHeader(hdr *tar.Header) {
	hdr.Format = tar.FormatPAX
	hdr.Uname = ""
	hdr.Gname = ""

	// Normalize paths so that the same entry always produces
	// the same header bytes regardless of how the tar was created.
	isDir := hdr.Typeflag == tar.TypeDir
	hdr.Name = canonicalizePath(hdr.Name, isDir)
	if hdr.Linkname != "" {
		hdr.Linkname = canonicalizePath(hdr.Linkname, false)
	}

	// Strip file type bits from Mode, keeping only permission + setuid/setgid/sticky.
	// Go's tar.Reader preserves file type bits from the archive (e.g. 040755 for dirs),
	// but tar.FileInfoHeader (used when creating tar from filesystem) only sets
	// permission bits since Go 1.9.  Normalizing avoids mismatches.
	hdr.Mode &= 0o7777

	// Zero out timestamps that can't be reliably preserved on the filesystem.
	// ChangeTime (ctime) is always set by the kernel when inode metadata changes,
	// so it will differ after extraction. AccessTime is also unreliable (depends
	// on mount options like noatime/relatime).
	hdr.AccessTime = time.Time{}
	hdr.ChangeTime = time.Time{}

	if hdr.PAXRecords == nil {
		hdr.PAXRecords = make(map[string]string)
	}

	// Keep only SCHILY.xattr.* records; drop everything else.
	for k := range hdr.PAXRecords {
		if !strings.HasPrefix(k, archive.PaxSchilyXattr) {
			delete(hdr.PAXRecords, k)
		}
	}

	// Add a non-basic PAX record to force PAX format.
	// Without this, Go's tar writer may fall back to USTAR for simple entries
	// even when Format is set to FormatPAX (see common.go:509-510).
	hdr.PAXRecords[canonicalPAXRecordKey] = "1"

	if hdr.Typeflag == tar.TypeLink {
		hdr.Size = 0
	}
}

// CanonicalHeaderBytes generates the canonical tar header bytes for a given
// tar header. This creates a scratch tar.Writer to produce the exact bytes.
// The returned bytes include any PAX extended header blocks and the main
// header block, but NOT file content or content padding.
func CanonicalHeaderBytes(hdr *tar.Header) ([]byte, error) {
	CanonicalizeTarHeader(hdr)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	// Don't call Flush() or Close() — we only want the header bytes.
	// The scratch writer is discarded.
	_ = tw
	return buf.Bytes(), nil
}

// CanonicalEndOfArchive returns the canonical end-of-archive marker
// (two 512-byte zero blocks = 1024 bytes).
func CanonicalEndOfArchive() []byte {
	return make([]byte, 1024)
}

// NewFileMetadata creates a basic FileMetadata entry for hdr.
// The caller must set DigestOffset/EndOffset, and the Chunk* values, separately.
func NewFileMetadata(hdr *tar.Header) (FileMetadata, error) {
	typ, err := GetType(hdr.Typeflag)
	if err != nil {
		return FileMetadata{}, err
	}
	xattrs := make(map[string]string)
	for k, v := range hdr.PAXRecords {
		xattrKey, ok := strings.CutPrefix(k, archive.PaxSchilyXattr)
		if !ok {
			continue
		}
		xattrs[xattrKey] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return FileMetadata{
		Type:       typ,
		Name:       hdr.Name,
		Linkname:   hdr.Linkname,
		Mode:       hdr.Mode,
		Size:       hdr.Size,
		UID:        hdr.Uid,
		GID:        hdr.Gid,
		ModTime:    timeIfNotZero(&hdr.ModTime),
		AccessTime: timeIfNotZero(&hdr.AccessTime),
		ChangeTime: timeIfNotZero(&hdr.ChangeTime),
		Devmajor:   hdr.Devmajor,
		Devminor:   hdr.Devminor,
		Xattrs:     xattrs,
	}, nil
}

// NewCanonicalTarFilter wraps a tar stream so that every header is
// canonicalized before it is re-emitted. The file content is passed
// through unchanged.
//
// This is used when Diff() produces a tar from the filesystem: the
// raw tar has non-canonical headers (no PAX forced, Uname set, file
// type bits in Mode, etc.). Wrapping it through this filter makes the
// output byte-identical to what WriteCanonicalTar produces from the
// TOC metadata, so the digest matches the stored UncompressedDigest.
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
