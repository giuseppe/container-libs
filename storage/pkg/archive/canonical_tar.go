package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
)

const (
	blockSize = 512

	nameSize = 100
	linkSize = 100

	paxMaxUID   int64 = 0o7777777 // 2097151
	paxMaxGID   int64 = 0o7777777
	paxMaxSize  int64 = 0o77777777777 // 8 GiB - 1
	paxMaxMtime int64 = 0o77777777777
)

// CanonicalTarWriter writes tar archives in the canonical format defined by
// the composefs canonical tar specification. It produces byte-reproducible
// output: ustar headers with no prefix/name split, pax extended headers
// only when ustar fields overflow, and a global pax header marking the
// archive as canonical.
type CanonicalTarWriter struct {
	w            io.Writer
	closed       bool
	globalDone   bool
	contentSize  int64
	bytesWritten int64
}

// NewCanonicalTarWriter creates a writer that produces canonical tar output.
func NewCanonicalTarWriter(w io.Writer) *CanonicalTarWriter {
	return &CanonicalTarWriter{w: w}
}

// NewCanonicalTarWriterRaw creates a writer that produces canonical tar
// headers without the global pax header.
func NewCanonicalTarWriterRaw(w io.Writer) *CanonicalTarWriter {
	return &CanonicalTarWriter{w: w, globalDone: true}
}

func (cw *CanonicalTarWriter) writeGlobalHeader() error {
	if cw.globalDone {
		return nil
	}
	cw.globalDone = true

	rec := formatPAXRecord("canonical-tar", "1")
	return cw.writeRawPAXEntry("GlobalHead.0.0", 'g', []byte(rec))
}

// WriteHeader writes a tar header in canonical format.
func (cw *CanonicalTarWriter) WriteHeader(h *tar.Header) error {
	if cw.closed {
		return fmt.Errorf("canonical tar: write to closed writer")
	}
	if err := cw.finishContent(); err != nil {
		return err
	}
	if err := cw.writeGlobalHeader(); err != nil {
		return err
	}

	paxData := cw.buildPAXRecords(h)
	if len(paxData) > 0 {
		baseName := path.Base(h.Name)
		if len(baseName) > nameSize {
			baseName = baseName[:nameSize]
		}
		paxName := "PaxHeaders.0/" + baseName
		if len(paxName) > nameSize {
			paxName = paxName[:nameSize]
		}
		if err := cw.writeRawPAXEntry(paxName, 'x', []byte(paxData)); err != nil {
			return err
		}
	}

	if err := cw.writeUSTARHeader(h); err != nil {
		return err
	}

	if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
		cw.contentSize = h.Size
		cw.bytesWritten = 0
	}

	return nil
}

// Write writes file content data.
func (cw *CanonicalTarWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := cw.contentSize - cw.bytesWritten
	if remaining <= 0 {
		return 0, fmt.Errorf("canonical tar: write without header")
	}
	if int64(len(p)) > remaining {
		return 0, fmt.Errorf("canonical tar: write too long")
	}

	n, err := cw.w.Write(p)
	cw.bytesWritten += int64(n)
	return n, err
}

// Close writes the end-of-archive marker (two zero blocks).
func (cw *CanonicalTarWriter) Close() error {
	if cw.closed {
		return nil
	}
	cw.closed = true

	if err := cw.finishContent(); err != nil {
		return err
	}
	if err := cw.writeGlobalHeader(); err != nil {
		return err
	}

	var zeros [blockSize * 2]byte
	_, err := cw.w.Write(zeros[:])
	return err
}

// Flush is a no-op for interface compatibility.
func (cw *CanonicalTarWriter) Flush() error {
	return nil
}

func (cw *CanonicalTarWriter) finishContent() error {
	if cw.contentSize > 0 && cw.bytesWritten < cw.contentSize {
		return fmt.Errorf("canonical tar: %d bytes of content not written", cw.contentSize-cw.bytesWritten)
	}
	if cw.bytesWritten > 0 {
		remainder := cw.bytesWritten % blockSize
		if remainder != 0 {
			padding := blockSize - remainder
			var zeros [blockSize]byte
			if _, err := cw.w.Write(zeros[:padding]); err != nil {
				return err
			}
		}
	}
	cw.contentSize = 0
	cw.bytesWritten = 0
	return nil
}

func (cw *CanonicalTarWriter) buildPAXRecords(h *tar.Header) string {
	var buf strings.Builder

	if len(h.Name) > nameSize {
		buf.WriteString(formatPAXRecord("path", h.Name))
	}

	if len(h.Linkname) > linkSize {
		buf.WriteString(formatPAXRecord("linkpath", h.Linkname))
	}

	if h.Size > paxMaxSize {
		buf.WriteString(formatPAXRecord("size", strconv.FormatInt(h.Size, 10)))
	}

	if int64(h.Uid) > paxMaxUID {
		buf.WriteString(formatPAXRecord("uid", strconv.Itoa(h.Uid)))
	}

	if int64(h.Gid) > paxMaxGID {
		buf.WriteString(formatPAXRecord("gid", strconv.Itoa(h.Gid)))
	}

	sec := h.ModTime.Unix()
	nsec := h.ModTime.Nanosecond()
	if sec > paxMaxMtime || nsec != 0 {
		var mtimeStr string
		if nsec != 0 {
			mtimeStr = fmt.Sprintf("%d.%s", sec, strings.TrimRight(fmt.Sprintf("%09d", nsec), "0"))
		} else {
			mtimeStr = strconv.FormatInt(sec, 10)
		}
		buf.WriteString(formatPAXRecord("mtime", mtimeStr))
	}

	xattrs := make(map[string]string)
	for k, v := range h.Xattrs { //nolint:staticcheck
		xattrs["SCHILY.xattr."+k] = v
	}
	for k, v := range h.PAXRecords {
		if strings.HasPrefix(k, "SCHILY.xattr.") {
			xattrs[k] = v
		}
	}
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range xattrs {
			if !yield(k) {
				return
			}
		}
	}) {
		buf.WriteString(formatPAXRecord(k, xattrs[k]))
	}

	return buf.String()
}

func (cw *CanonicalTarWriter) writeRawPAXEntry(name string, typeflag byte, data []byte) error {
	var hdr [blockSize]byte
	canonicalFormatString(hdr[0:100], name)
	canonicalFormatOctal(hdr[100:108], 0)
	canonicalFormatOctal(hdr[108:116], 0)
	canonicalFormatOctal(hdr[116:124], 0)
	canonicalFormatOctal(hdr[124:136], int64(len(data)))
	canonicalFormatOctal(hdr[136:148], 0)
	hdr[156] = typeflag
	copy(hdr[257:263], "ustar\x00")
	copy(hdr[263:265], "00")
	canonicalComputeChecksum(hdr[:])

	if _, err := cw.w.Write(hdr[:]); err != nil {
		return err
	}
	return cw.writeDataBlocks(data)
}

func (cw *CanonicalTarWriter) writeUSTARHeader(h *tar.Header) error {
	var hdr [blockSize]byte

	name := h.Name
	if len(name) > nameSize {
		name = name[:nameSize]
	}
	canonicalFormatString(hdr[0:100], name)

	canonicalFormatOctal(hdr[100:108], int64(h.Mode))
	canonicalFormatOctal(hdr[108:116], int64(h.Uid))
	canonicalFormatOctal(hdr[116:124], int64(h.Gid))

	sz := h.Size
	switch h.Typeflag {
	case tar.TypeLink, tar.TypeSymlink, tar.TypeDir,
		tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		sz = 0
	}
	canonicalFormatOctal(hdr[124:136], sz)

	mtime := h.ModTime.Unix()
	if mtime < 0 {
		mtime = 0
	}
	canonicalFormatOctal(hdr[136:148], mtime)

	switch h.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		hdr[156] = '0'
	case tar.TypeLink:
		hdr[156] = '1'
	case tar.TypeSymlink:
		hdr[156] = '2'
	case tar.TypeChar:
		hdr[156] = '3'
	case tar.TypeBlock:
		hdr[156] = '4'
	case tar.TypeDir:
		hdr[156] = '5'
	case tar.TypeFifo:
		hdr[156] = '6'
	default:
		hdr[156] = h.Typeflag
	}

	linkname := h.Linkname
	if len(linkname) > linkSize {
		linkname = linkname[:linkSize]
	}
	canonicalFormatString(hdr[157:257], linkname)

	copy(hdr[257:263], "ustar\x00")
	copy(hdr[263:265], "00")

	// uname (265:297) and gname (297:329) left as zeros (empty per spec).

	if h.Typeflag == tar.TypeBlock || h.Typeflag == tar.TypeChar {
		canonicalFormatOctal(hdr[329:337], h.Devmajor)
		canonicalFormatOctal(hdr[337:345], h.Devminor)
	} else {
		canonicalFormatOctal(hdr[329:337], 0)
		canonicalFormatOctal(hdr[337:345], 0)
	}

	// prefix (345:500) is always empty — the canonical format never uses
	// the ustar prefix/name split.

	canonicalComputeChecksum(hdr[:])

	_, err := cw.w.Write(hdr[:])
	return err
}

func (cw *CanonicalTarWriter) writeDataBlocks(data []byte) error {
	if _, err := cw.w.Write(data); err != nil {
		return err
	}
	remainder := len(data) % blockSize
	if remainder != 0 {
		padding := blockSize - remainder
		var zeros [blockSize]byte
		if _, err := cw.w.Write(zeros[:padding]); err != nil {
			return err
		}
	}
	return nil
}

// canonicalFormatString writes s into b, null-terminated with zero-fill.
func canonicalFormatString(b []byte, s string) {
	n := copy(b, s)
	for i := n; i < len(b); i++ {
		b[i] = 0
	}
}

// canonicalFormatOctal writes x as zero-padded null-terminated octal into b.
func canonicalFormatOctal(b []byte, x int64) {
	s := strconv.FormatInt(x, 8)
	n := len(b) - len(s) - 1
	i := 0
	for ; i < n; i++ {
		b[i] = '0'
	}
	i += copy(b[i:], s)
	for ; i < len(b); i++ {
		b[i] = 0
	}
}

// canonicalComputeChecksum calculates and writes the tar header checksum.
func canonicalComputeChecksum(hdr []byte) {
	for i := 148; i < 156; i++ {
		hdr[i] = ' '
	}
	var sum uint64
	for _, b := range hdr[:blockSize] {
		sum += uint64(b)
	}
	s := fmt.Sprintf("%06o", sum)
	copy(hdr[148:154], s)
	hdr[154] = 0
	hdr[155] = ' '
}

// formatPAXRecord formats a single pax record as "<length> <key>=<value>\n".
func formatPAXRecord(key, value string) string {
	const padding = 3 // " " + "=" + "\n"
	size := len(key) + len(value) + padding
	size += len(strconv.Itoa(size))
	record := strconv.Itoa(size) + " " + key + "=" + value + "\n"
	// Adding the length prefix may increase the total length by one digit.
	if len(record) != size {
		size = len(record)
		record = strconv.Itoa(size) + " " + key + "=" + value + "\n"
	}
	return record
}

var _ interface {
	WriteHeader(*tar.Header) error
	Write([]byte) (int, error)
	Close() error
	Flush() error
} = (*CanonicalTarWriter)(nil)
