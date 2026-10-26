package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func verifyDirectory(dir, version, commit string) error {
	if !versionPattern.MatchString(version) || len(version) > 80 {
		return errors.New("verification requires the expected --version")
	}
	if commit != "" && !commitPattern.MatchString(commit) {
		return errors.New("invalid expected commit")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("package directory must be a real directory")
	}
	metadata, err := readRegular(filepath.Join(dir, "manifest.json"), maxMetadataBytes)
	if err != nil {
		return err
	}
	var m manifest
	if err = strictJSON(metadata, &m); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if m.Schema != 1 || m.Product != "Relay" || m.Version != version || !strings.HasPrefix(m.Signing, "UNSIGNED:") {
		return errors.New("manifest schema/product/version/unsigned label mismatch")
	}
	if commit != "" && m.Commit != commit {
		return errors.New("manifest commit mismatch")
	}
	if err = validateRecord(m.record); err != nil {
		return err
	}
	if len(m.Artifacts) < 1 || len(m.Artifacts) > 2 || len(m.Toolchains) > 12 || len(m.Limitations) > 12 {
		return errors.New("invalid manifest counts")
	}
	expected := map[string]string{"manifest.json": hexHash(metadata)}
	formats := map[string]bool{}
	for _, a := range m.Artifacts {
		if !expectedArtifact(m.record, a) || a.Size <= 0 || a.Size > maxArtifactBytes || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a.SHA256) {
			return errors.New("invalid artifact name, format, size or digest")
		}
		if _, ok := expected[a.Name]; ok {
			return errors.New("duplicate artifact")
		}
		if formats[a.Format] {
			return errors.New("duplicate artifact format")
		}
		formats[a.Format] = true
		info, e := os.Lstat(filepath.Join(dir, a.Name))
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() != a.Size {
			return errors.New("artifact is not a regular file or size mismatch")
		}
		digest, e := fileHash(filepath.Join(dir, a.Name))
		if e != nil {
			return e
		}
		if digest != a.SHA256 {
			return errors.New("artifact checksum mismatch")
		}
		expected[a.Name] = digest
		if e = verifyArchive(filepath.Join(dir, a.Name), a.Format, m.record); e != nil {
			return fmt.Errorf("%s: %w", a.Name, e)
		}
	}
	if strings.HasPrefix(m.Target, "linux/") && (!formats["deb"] || !formats["tar.gz"]) {
		return errors.New("Linux release requires portable tar and deb")
	}
	checks, err := readRegular(filepath.Join(dir, "SHA256SUMS"), maxMetadataBytes)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	scan := bufio.NewScanner(bytes.NewReader(checks))
	for scan.Scan() {
		line := scan.Text()
		if len(line) < 67 || line[64:66] != "  " {
			return errors.New("invalid SHA256SUMS line")
		}
		digest, name := line[:64], line[66:]
		if seen[name] || expected[name] == "" || expected[name] != digest {
			return errors.New("checksum list mismatch or duplicate")
		}
		seen[name] = true
	}
	if err = scan.Err(); err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("checksum list is incomplete")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "SHA256SUMS" && expected[e.Name()] == "" {
			return errors.New("unexpected file in release directory")
		}
		if e.Type()&os.ModeSymlink != 0 || e.IsDir() {
			return errors.New("release directory contains links or subdirectories")
		}
	}
	return nil
}

func expectedArtifact(r record, a artifact) bool {
	stem := assetStem(r)
	switch strings.Split(r.Target, "/")[0] {
	case "linux":
		return a.Format == "tar.gz" && a.Name == stem+".tar.gz" || a.Format == "deb" && a.Name == stem+".deb"
	case "windows":
		return a.Format == "zip" && a.Name == stem+".zip"
	case "darwin":
		return a.Format == "app-tar.gz" && a.Name == stem+".tar.gz"
	}
	return false
}
func readRegular(name string, limit int64) ([]byte, error) {
	info, e := os.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("file must be regular and within the size limit")
	}
	f, e := os.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, e
}

func safeArchiveName(name string) bool {
	return name != "" && len(name) <= 240 && !strings.ContainsAny(name, "\\:\x00\r\n") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}

type archiveCheck struct {
	record   record
	expected map[string]bool
	seen     map[string]bool
	metadata bool
	binary   bool
	total    int64
	count    int
}

func newArchiveCheck(r record, format string) *archiveCheck {
	names := []string{}
	switch format {
	case "zip":
		names = []string{"LICENSE", "README.txt", "RELEASE.json", "relay.exe"}
	case "tar.gz":
		names = []string{"relay/LICENSE", "relay/README.txt", "relay/RELEASE.json", "relay/relay", "relay/relay.desktop", "relay/relay.svg"}
	case "app-tar.gz":
		names = []string{"LICENSE", "README.txt", "Relay.app/Contents/Info.plist", "Relay.app/Contents/MacOS/relay", "Relay.app/Contents/Resources/RELEASE.json"}
	case "deb-data":
		names = []string{"usr/bin/relay", "usr/share/applications/relay.desktop", "usr/share/icons/hicolor/scalable/apps/relay.svg", "usr/share/doc/relay/copyright", "usr/share/doc/relay/README.txt", "usr/share/doc/relay/RELEASE.json"}
	case "deb-control":
		names = []string{"control"}
	}
	expected := map[string]bool{}
	for _, n := range names {
		expected[n] = true
	}
	return &archiveCheck{record: r, expected: expected, seen: map[string]bool{}}
}
func (c *archiveCheck) accept(name string, size int64, mode os.FileMode, reader io.Reader) error {
	c.count++
	c.total += size
	if !safeArchiveName(name) || c.seen[name] || !c.expected[name] || c.count > 10000 || size < 0 || size > maxArtifactBytes || c.total > maxArchiveBytes {
		return errors.New("unsafe, duplicate, unexpected or oversized archive entry")
	}
	if !mode.IsRegular() || mode.Perm()&0022 != 0 || mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("archive contains a link, special file or unsafe mode")
	}
	c.seen[name] = true
	if path.Base(name) == "RELEASE.json" {
		if size > maxMetadataBytes {
			return errors.New("release metadata is too large")
		}
		data, e := io.ReadAll(io.LimitReader(reader, maxMetadataBytes+1))
		if e != nil {
			return e
		}
		var r record
		if e = strictJSON(data, &r); e != nil {
			return e
		}
		if r != c.record {
			return errors.New("embedded release version/commit/target/epoch mismatch")
		}
		c.metadata = true
	} else if name == "relay.exe" || name == "relay/relay" || name == "usr/bin/relay" || name == "Relay.app/Contents/MacOS/relay" {
		if mode.Perm() != 0755 || size < 4 {
			return errors.New("invalid executable mode or size")
		}
		magic := make([]byte, 4)
		if _, e := io.ReadFull(reader, magic); e != nil {
			return e
		}
		target := strings.Split(c.record.Target, "/")[0]
		valid := target == "linux" && bytes.Equal(magic, []byte{0x7f, 'E', 'L', 'F'}) || target == "windows" && bytes.Equal(magic[:2], []byte{'M', 'Z'}) || target == "darwin" && (bytes.Equal(magic, []byte{0xcf, 0xfa, 0xed, 0xfe}) || bytes.Equal(magic, []byte{0xfe, 0xed, 0xfa, 0xcf}) || bytes.Equal(magic, []byte{0xca, 0xfe, 0xba, 0xbe}))
		if !valid {
			return errors.New("executable format does not match target")
		}
		c.binary = true
	} else if name == "control" {
		if size > 64<<10 {
			return errors.New("deb control is too large")
		}
		data, e := io.ReadAll(io.LimitReader(reader, 64<<10+1))
		if e != nil {
			return e
		}
		text := string(data)
		if !strings.Contains(text, "\nVersion: "+strings.ReplaceAll(c.record.Version, "-", "~")+"\n") || !strings.Contains(text, "\nArchitecture: "+strings.Split(c.record.Target, "/")[1]+"\n") || !strings.HasPrefix(text, "Package: relay\n") {
			return errors.New("deb control metadata mismatch")
		}
	} else if name == "relay/relay.desktop" || name == "usr/share/applications/relay.desktop" {
		data, e := io.ReadAll(io.LimitReader(reader, 64<<10+1))
		if e != nil {
			return e
		}
		if len(data) > 64<<10 || !strings.Contains(string(data), "\nExec=relay\n") || !strings.Contains(string(data), "\nIcon=relay\n") {
			return errors.New("unsafe desktop launch command")
		}
	}
	_, err := io.Copy(io.Discard, reader)
	return err
}
func (c *archiveCheck) finish(control bool) error {
	if len(c.seen) != len(c.expected) {
		return errors.New("archive inventory is incomplete")
	}
	if !control && (!c.binary || !c.metadata) {
		return errors.New("archive executable or release metadata is missing")
	}
	return nil
}

func verifyArchive(name, format string, r record) error {
	if format == "zip" {
		zr, err := zip.OpenReader(name)
		if err != nil {
			return err
		}
		defer zr.Close()
		if len(zr.File) > 10000 {
			return errors.New("too many zip entries")
		}
		if err = verifyZIPHeaders(name, zr.File); err != nil {
			return err
		}
		c := newArchiveCheck(r, format)
		for _, f := range zr.File {
			if f.UncompressedSize64 > uint64(maxArtifactBytes) {
				return errors.New("zip entry size limit exceeded")
			}
			rd, e := f.Open()
			if e != nil {
				return e
			}
			e = c.accept(f.Name, int64(f.UncompressedSize64), f.Mode(), io.LimitReader(rd, maxArtifactBytes+1))
			closeErr := rd.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return c.finish(false)
	}
	if format == "deb" {
		return verifyDEB(name, r)
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return verifyTar(f, r, format)
}

// Validate both sets of ZIP names. archive/zip intentionally trusts the central
// directory, while some extraction tools also inspect local file headers.
// These release ZIPs have no SFX prefix, ZIP64 entries or archive comment.
func verifyZIPHeaders(name string, files []*zip.File) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 22 {
		return errors.New("truncated zip")
	}
	end := make([]byte, 22)
	if _, err = f.ReadAt(end, info.Size()-22); err != nil {
		return err
	}
	u16 := binary.LittleEndian.Uint16
	u32 := binary.LittleEndian.Uint32
	if u32(end[:4]) != 0x06054b50 || u16(end[4:6]) != 0 || u16(end[6:8]) != 0 || u16(end[8:10]) != uint16(len(files)) || u16(end[10:12]) != uint16(len(files)) || u16(end[20:22]) != 0 {
		return errors.New("unsupported or trailing zip directory data")
	}
	var offset int64
	for _, entry := range files {
		h := make([]byte, 30)
		if _, err = f.ReadAt(h, offset); err != nil {
			return err
		}
		if u32(h[:4]) != 0x04034b50 || u16(h[6:8]) != entry.Flags || u16(h[8:10]) != entry.Method {
			return errors.New("zip local header mismatch")
		}
		nlen, xlen := int64(u16(h[26:28])), int64(u16(h[28:30]))
		if nlen > 240 {
			return errors.New("zip local name is too long")
		}
		localName := make([]byte, nlen)
		if _, err = f.ReadAt(localName, offset+30); err != nil {
			return err
		}
		if string(localName) != entry.Name || !safeArchiveName(string(localName)) {
			return errors.New("unsafe or mismatched zip local filename")
		}
		body, err := entry.DataOffset()
		if err != nil {
			return err
		}
		if body != offset+30+nlen+xlen {
			return errors.New("zip has overlapping or non-canonical entries")
		}
		if entry.CompressedSize64 > uint64(maxArtifactBytes) || entry.UncompressedSize64 > uint64(maxArtifactBytes) {
			return errors.New("zip entry size limit exceeded")
		}
		offset = body + int64(entry.CompressedSize64)
		if entry.Flags&8 != 0 {
			descriptor := make([]byte, 16)
			if _, err = f.ReadAt(descriptor, offset); err != nil {
				return err
			}
			if u32(descriptor[:4]) != 0x08074b50 || u32(descriptor[4:8]) != entry.CRC32 || uint64(u32(descriptor[8:12])) != entry.CompressedSize64 || uint64(u32(descriptor[12:16])) != entry.UncompressedSize64 {
				return errors.New("zip data descriptor mismatch")
			}
			offset += 16
		} else if u32(h[14:18]) != entry.CRC32 || uint64(u32(h[18:22])) != entry.CompressedSize64 || uint64(u32(h[22:26])) != entry.UncompressedSize64 {
			return errors.New("zip local size or CRC mismatch")
		}
	}
	if offset != int64(u32(end[16:20])) || offset+int64(u32(end[12:16])) != info.Size()-22 {
		return errors.New("zip central directory offset mismatch")
	}
	return nil
}
func verifyTar(reader io.Reader, r record, format string) error {
	buffered := bufio.NewReader(reader)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return err
	}
	defer gz.Close()
	gz.Multistream(false)
	limited := &io.LimitedReader{R: gz, N: maxArchiveBytes + 1}
	tr := tar.NewReader(limited)
	c := newArchiveCheck(r, format)
	for {
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return errors.New("tar links, directories and special entries are not allowed")
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			return errors.New("non-canonical tar ownership")
		}
		if e = c.accept(header.Name, header.Size, header.FileInfo().Mode(), tr); e != nil {
			return e
		}
	}
	remaining, err := io.Copy(io.Discard, limited)
	if err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("expanded archive limit exceeded")
	}
	if remaining != 0 {
		return errors.New("trailing decompressed tar content")
	}
	if _, err = buffered.ReadByte(); err != io.EOF {
		return errors.New("trailing or concatenated gzip content")
	}
	return c.finish(format == "deb-control")
}
func verifyDEB(name string, r record) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 8)
	if _, err = io.ReadFull(f, magic); err != nil || string(magic) != "!<arch>\n" {
		return errors.New("invalid deb ar header")
	}
	names := []string{"debian-binary", "control.tar.gz", "data.tar.gz"}
	for _, expected := range names {
		header := make([]byte, 60)
		if _, err = io.ReadFull(f, header); err != nil {
			return err
		}
		if string(header[58:]) != "`\n" || strings.TrimSpace(string(header[:16])) != expected+"/" {
			return errors.New("unexpected, duplicate or unsafe deb member")
		}
		if strings.TrimSpace(string(header[16:28])) != strconv.FormatInt(r.SourceDateEpoch, 10) || strings.TrimSpace(string(header[28:34])) != "0" || strings.TrimSpace(string(header[34:40])) != "0" || strings.TrimSpace(string(header[40:48])) != "644" {
			return errors.New("invalid deb member date, ownership or mode")
		}
		size, e := strconv.ParseInt(strings.TrimSpace(string(header[48:58])), 10, 64)
		if e != nil || size < 0 || size > maxArtifactBytes {
			return errors.New("invalid deb member size")
		}
		lr := &io.LimitedReader{R: f, N: size}
		if expected == "debian-binary" {
			v, e := io.ReadAll(lr)
			if e != nil || string(v) != "2.0\n" {
				return errors.New("invalid deb version")
			}
		} else {
			format := "deb-data"
			if expected == "control.tar.gz" {
				format = "deb-control"
			}
			if e = verifyTar(lr, r, format); e != nil {
				return e
			}
		}
		if lr.N != 0 {
			return errors.New("truncated deb member")
		}
		if size%2 != 0 {
			pad := make([]byte, 1)
			if _, err = io.ReadFull(f, pad); err != nil || pad[0] != '\n' {
				return errors.New("invalid deb padding")
			}
		}
	}
	trailing := make([]byte, 1)
	if n, e := f.Read(trailing); n != 0 || e != io.EOF {
		return errors.New("trailing deb members or data")
	}
	return nil
}
