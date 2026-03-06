package archive

import (
	"archive/tar"
	"bytes"
	"testing"
	"time"
)

func TestCanonicalizeTarHeader(t *testing.T) {
	modTime := time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC)

	tests := []struct {
		name     string
		input    *tar.Header
		expected *tar.Header
	}{
		{
			name: "regular file with type bits in mode",
			input: &tar.Header{
				Name:       "./usr/bin/test",
				Typeflag:   tar.TypeReg,
				Mode:       0o100755,
				Size:       42,
				Uid:        1000,
				Gid:        1000,
				Uname:      "user",
				Gname:      "group",
				ModTime:    modTime,
				AccessTime: time.Now(),
				ChangeTime: time.Now(),
			},
			expected: &tar.Header{
				Name:     "usr/bin/test",
				Typeflag: tar.TypeReg,
				Mode:     0o755,
				Size:     42,
				Uid:      1000,
				Gid:      1000,
				ModTime:  modTime,
				Format:   tar.FormatPAX,
				PAXRecords: map[string]string{
					PaxCanonical: "1",
				},
			},
		},
		{
			name: "directory gets trailing slash",
			input: &tar.Header{
				Name:     "./usr/lib",
				Typeflag: tar.TypeDir,
				Mode:     0o40755,
			},
			expected: &tar.Header{
				Name:     "usr/lib/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
				Format:   tar.FormatPAX,
				PAXRecords: map[string]string{
					PaxCanonical: "1",
				},
			},
		},
		{
			name: "hardlink size zeroed",
			input: &tar.Header{
				Name:     "usr/bin/link",
				Typeflag: tar.TypeLink,
				Linkname: "./usr/bin/target",
				Size:     100,
				Mode:     0o755,
			},
			expected: &tar.Header{
				Name:     "usr/bin/link",
				Typeflag: tar.TypeLink,
				Linkname: "usr/bin/target",
				Size:     0,
				Mode:     0o755,
				Format:   tar.FormatPAX,
				PAXRecords: map[string]string{
					PaxCanonical: "1",
				},
			},
		},
		{
			name: "only xattr PAX records preserved",
			input: &tar.Header{
				Name:     "file.txt",
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				PAXRecords: map[string]string{
					"SCHILY.xattr.security.selinux": "system_u:object_r:bin_t:s0\x00",
					"mtime":                         "1234567890.123",
					"atime":                         "1234567890.456",
					"comment":                       "test",
				},
			},
			expected: &tar.Header{
				Name:     "file.txt",
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				Format:   tar.FormatPAX,
				PAXRecords: map[string]string{
					PaxCanonical:                    "1",
					"SCHILY.xattr.security.selinux": "system_u:object_r:bin_t:s0\x00",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			CanonicalizeTarHeader(tt.input)

			if tt.input.Name != tt.expected.Name {
				t.Errorf("Name: got %q, want %q", tt.input.Name, tt.expected.Name)
			}
			if tt.input.Mode != tt.expected.Mode {
				t.Errorf("Mode: got %o, want %o", tt.input.Mode, tt.expected.Mode)
			}
			if tt.input.Format != tt.expected.Format {
				t.Errorf("Format: got %v, want %v", tt.input.Format, tt.expected.Format)
			}
			if tt.input.Uname != "" {
				t.Errorf("Uname not cleared: %q", tt.input.Uname)
			}
			if tt.input.Gname != "" {
				t.Errorf("Gname not cleared: %q", tt.input.Gname)
			}
			if !tt.input.AccessTime.IsZero() {
				t.Errorf("AccessTime not zeroed: %v", tt.input.AccessTime)
			}
			if !tt.input.ChangeTime.IsZero() {
				t.Errorf("ChangeTime not zeroed: %v", tt.input.ChangeTime)
			}
			if tt.input.Size != tt.expected.Size {
				t.Errorf("Size: got %d, want %d", tt.input.Size, tt.expected.Size)
			}
			if tt.input.Linkname != tt.expected.Linkname {
				t.Errorf("Linkname: got %q, want %q", tt.input.Linkname, tt.expected.Linkname)
			}
			if tt.input.PAXRecords[PaxCanonical] != "1" {
				t.Errorf("missing %s=1 PAX record", PaxCanonical)
			}
			for k := range tt.expected.PAXRecords {
				if tt.input.PAXRecords[k] != tt.expected.PAXRecords[k] {
					t.Errorf("PAXRecords[%q]: got %q, want %q", k, tt.input.PAXRecords[k], tt.expected.PAXRecords[k])
				}
			}
			for k := range tt.input.PAXRecords {
				if _, ok := tt.expected.PAXRecords[k]; !ok {
					t.Errorf("unexpected PAX record %q=%q", k, tt.input.PAXRecords[k])
				}
			}
		})
	}
}

func TestIsCanonicalHeader(t *testing.T) {
	canonical := &tar.Header{
		Name:     "usr/bin/test",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
		Size:     42,
		Format:   tar.FormatPAX,
		PAXRecords: map[string]string{
			PaxCanonical: "1",
		},
	}
	if err := IsCanonicalHeader(canonical); err != nil {
		t.Errorf("expected canonical header to pass, got: %v", err)
	}

	nonCanonical := []struct {
		name   string
		modify func(*tar.Header)
	}{
		{"missing PAX record", func(h *tar.Header) { delete(h.PAXRecords, PaxCanonical) }},
		{"non-empty Uname", func(h *tar.Header) { h.Uname = "root" }},
		{"non-empty Gname", func(h *tar.Header) { h.Gname = "root" }},
		{"type bits in mode", func(h *tar.Header) { h.Mode = 0o100755 }},
		{"non-zero AccessTime", func(h *tar.Header) { h.AccessTime = time.Now() }},
		{"non-zero ChangeTime", func(h *tar.Header) { h.ChangeTime = time.Now() }},
		{"non-xattr PAX record", func(h *tar.Header) { h.PAXRecords["comment"] = "test" }},
		{"hardlink with size", func(h *tar.Header) { h.Typeflag = tar.TypeLink; h.Size = 100 }},
		{"non-canonical path", func(h *tar.Header) { h.Name = "./usr/bin/test" }},
	}

	for _, tc := range nonCanonical {
		t.Run(tc.name, func(t *testing.T) {
			h := &tar.Header{
				Name:     "usr/bin/test",
				Typeflag: tar.TypeReg,
				Mode:     0o755,
				Size:     42,
				Format:   tar.FormatPAX,
				PAXRecords: map[string]string{
					PaxCanonical: "1",
				},
			}
			tc.modify(h)
			if err := IsCanonicalHeader(h); err == nil {
				t.Error("expected non-canonical header to be rejected")
			}
		})
	}
}

func TestComparePathComponents(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a", 0},
		{"usr/bin/test", "usr/lib/test", -1},
		{"usr/lib/test", "usr/bin/test", 1},
		// Directory (trailing /) sorts before files in that directory
		{"usr/", "usr/bin", -1},
		// gnus/ before gnus-tut.txt (component comparison: "gnus" with "/" vs "gnus-tut.txt")
		{"gnus/", "gnus-tut.txt", -1},
		{"gnus-tut.txt", "gnus/", 1},
		// Same prefix, different depths
		{"a/b", "a/b/c", -1},
		{"a/b/c", "a/b", 1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			got := ComparePathComponents(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("ComparePathComponents(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCanonicalTarDeterminism(t *testing.T) {
	// Verify that canonicalizing the same header twice produces identical output.
	makeHeader := func() *tar.Header {
		return &tar.Header{
			Name:       "./usr/bin/test",
			Typeflag:   tar.TypeReg,
			Mode:       0o100755,
			Size:       5,
			Uid:        1000,
			Gid:        1000,
			Uname:      "user",
			Gname:      "group",
			ModTime:    time.Date(2024, 1, 15, 10, 30, 0, 123456789, time.UTC),
			AccessTime: time.Now(),
			ChangeTime: time.Now(),
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.selinux": "test\x00",
				"mtime":                         "1234567890.123",
			},
		}
	}

	writeTar := func() []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		hdr := makeHeader()
		CanonicalizeTarHeader(hdr)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	tar1 := writeTar()
	tar2 := writeTar()

	if !bytes.Equal(tar1, tar2) {
		t.Error("canonical tar output is not deterministic")
	}
}
