package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testRecord = record{Version: "0.1.0-rc.1", Commit: strings.Repeat("a", 40), SourceDateEpoch: 1792886400, Target: "linux/amd64", Unsigned: true}

func TestRecordRejectsUnsafeReleaseMetadata(t *testing.T) {
	for _, change := range []func(*record){func(r *record) { r.Version = "../../pwn" }, func(r *record) { r.Version = "v1.0.0" }, func(r *record) { r.Version = "1.0.0+unsafe" }, func(r *record) { r.Version = "01.0.0" }, func(r *record) { r.Commit = "abcdef" }, func(r *record) { r.SourceDateEpoch = 0 }, func(r *record) { r.SourceDateEpoch = 4354819200 }, func(r *record) { r.Target = "linux/../../amd64" }, func(r *record) { r.Unsigned = false }} {
		r := testRecord
		change(&r)
		if validateRecord(r) == nil {
			t.Fatalf("accepted unsafe release: %+v", r)
		}
	}
	if err := validateRecord(testRecord); err != nil {
		t.Fatal(err)
	}
}

func fixtureRelease(t *testing.T, target string) (string, manifest) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "LICENSE"), []byte("test license\n"))
	if e := os.Mkdir(filepath.Join(root, "build"), 0755); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(root, "build", "relay.desktop"), []byte("[Desktop Entry]\nExec=relay\nIcon=relay\n"))
	mustWrite(t, filepath.Join(root, "build", "relay.svg"), []byte("<svg/>"))
	r := testRecord
	r.Target = target
	magic := []byte{0x7f, 'E', 'L', 'F'}
	if strings.HasPrefix(target, "windows/") {
		magic = []byte{'M', 'Z', 0, 0}
	}
	if strings.HasPrefix(target, "darwin/") {
		magic = []byte{0xcf, 0xfa, 0xed, 0xfe}
	}
	binary := filepath.Join(root, "binary")
	mustWrite(t, binary, append(magic, bytes.Repeat([]byte("samplebinary"), 200)...))
	out := t.TempDir()
	as, err := packageRelease(root, binary, out, r)
	if err != nil {
		t.Fatal(err)
	}
	m := manifest{Schema: 1, Product: "Relay", record: r, Signing: "UNSIGNED: test only", SourceState: "working tree not authenticated by the base commit", Toolchains: map[string]string{"go": "test"}, Limitations: []string{"unsigned"}, Artifacts: as}
	for i := range m.Artifacts {
		a := &m.Artifacts[i]
		info, e := os.Stat(filepath.Join(out, a.Name))
		if e != nil {
			t.Fatal(e)
		}
		a.Size = info.Size()
		a.SHA256, e = fileHash(filepath.Join(out, a.Name))
		if e != nil {
			t.Fatal(e)
		}
	}
	writeMetadata(t, out, m)
	return out, m
}
func mustWrite(t *testing.T, name string, data []byte) {
	t.Helper()
	if e := os.WriteFile(name, data, 0644); e != nil {
		t.Fatal(e)
	}
}
func writeMetadata(t *testing.T, out string, m manifest) {
	t.Helper()
	data, e := json.MarshalIndent(m, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(out, "manifest.json"), data)
	checks := hexHash(data) + "  manifest.json\n"
	for _, a := range m.Artifacts {
		checks += a.SHA256 + "  " + a.Name + "\n"
	}
	mustWrite(t, filepath.Join(out, "SHA256SUMS"), []byte(checks))
}

func TestPackagesAreDeterministicAndVerifyWithoutExecution(t *testing.T) {
	for _, target := range []string{"linux/amd64", "windows/amd64", "darwin/arm64"} {
		t.Run(target, func(t *testing.T) {
			first, m := fixtureRelease(t, target)
			second, m2 := fixtureRelease(t, target)
			if err := verifyDirectory(first, m.Version, m.Commit); err != nil {
				t.Fatal(err)
			}
			for i, a := range m.Artifacts {
				if a.SHA256 != m2.Artifacts[i].SHA256 {
					t.Fatalf("archive is not deterministic: %s", a.Name)
				}
				b1, e := os.ReadFile(filepath.Join(first, a.Name))
				if e != nil {
					t.Fatal(e)
				}
				b2, e := os.ReadFile(filepath.Join(second, a.Name))
				if e != nil {
					t.Fatal(e)
				}
				if !bytes.Equal(b1, b2) {
					t.Fatal("archive bytes differ")
				}
			}
		})
	}
}

func TestVerificationRejectsTamperingAndUnexpectedAssets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *manifest)
	}{
		{"checksum", func(t *testing.T, d string, m *manifest) {
			p := filepath.Join(d, m.Artifacts[0].Name)
			data, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			data[len(data)/2] ^= 1
			mustWrite(t, p, data)
		}},
		{"size", func(t *testing.T, d string, m *manifest) { m.Artifacts[0].Size++; writeMetadata(t, d, *m) }},
		{"name", func(t *testing.T, d string, m *manifest) {
			m.Artifacts[0].Name = "../host.exe"
			writeMetadata(t, d, *m)
		}},
		{"version", func(t *testing.T, d string, m *manifest) { m.Version = "0.2.0"; writeMetadata(t, d, *m) }},
		{"embedded version", func(t *testing.T, d string, m *manifest) { m.Commit = strings.Repeat("b", 40); writeMetadata(t, d, *m) }},
		{"duplicate artifact", func(t *testing.T, d string, m *manifest) {
			m.Artifacts = append(m.Artifacts, m.Artifacts[0])
			writeMetadata(t, d, *m)
		}},
		{"missing checksum", func(t *testing.T, d string, m *manifest) { mustWrite(t, filepath.Join(d, "SHA256SUMS"), nil) }},
		{"duplicate checksum", func(t *testing.T, d string, m *manifest) {
			data, e := os.ReadFile(filepath.Join(d, "SHA256SUMS"))
			if e != nil {
				t.Fatal(e)
			}
			mustWrite(t, filepath.Join(d, "SHA256SUMS"), append(data, data...))
		}},
		{"unexpected executable", func(t *testing.T, d string, m *manifest) {
			mustWrite(t, filepath.Join(d, "updater.exe"), []byte("never execute"))
		}},
		{"symlink", func(t *testing.T, d string, m *manifest) {
			p := filepath.Join(d, m.Artifacts[0].Name)
			if e := os.Rename(p, p+".real"); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(p+".real", p); e != nil {
				t.Skip(e)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, m := fixtureRelease(t, "windows/amd64")
			version := m.Version
			tc.mutate(t, dir, &m)
			if verifyDirectory(dir, version, "") == nil {
				t.Fatal("tampered release accepted")
			}
		})
	}
}

func maliciousTar(t *testing.T, headers []*tar.Header, payload []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if e := tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if h.Size > 0 {
			if _, e := tw.Write(payload[:h.Size]); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestRejectsArchiveTraversalLinksDuplicatesAndCorruption(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "C:/drive", "a\\windows", "relay/../relay", "./relay", "a\x00b"} {
		if safeArchiveName(name) {
			t.Fatalf("unsafe path accepted: %q", name)
		}
	}
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}}},
		{"symlink", []*tar.Header{{Name: "relay/relay", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh", Mode: 0755}}},
		{"hardlink", []*tar.Header{{Name: "relay/relay", Typeflag: tar.TypeLink, Linkname: "/bin/sh", Mode: 0755}}},
		{"duplicate", []*tar.Header{{Name: "relay/LICENSE", Typeflag: tar.TypeReg, Mode: 0644}, {Name: "relay/LICENSE", Typeflag: tar.TypeReg, Mode: 0644}}},
		{"world writable", []*tar.Header{{Name: "relay/LICENSE", Typeflag: tar.TypeReg, Mode: 0666}}},
		{"setuid", []*tar.Header{{Name: "relay/LICENSE", Typeflag: tar.TypeReg, Mode: 04644}}},
		{"ownership", []*tar.Header{{Name: "relay/LICENSE", Typeflag: tar.TypeReg, Mode: 0644, Uid: 1000}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := maliciousTar(t, tc.headers, nil)
			if verifyTar(bytes.NewReader(data), testRecord, "tar.gz") == nil {
				t.Fatal("malicious archive accepted")
			}
		})
	}
	dir, m := fixtureRelease(t, "linux/amd64")
	var a artifact
	for _, item := range m.Artifacts {
		if item.Format == "tar.gz" {
			a = item
		}
	}
	data, e := os.ReadFile(filepath.Join(dir, a.Name))
	if e != nil {
		t.Fatal(e)
	}
	for _, corrupt := range [][]byte{data[:len(data)-4], append(append([]byte{}, data...), []byte("trailer")...), append(append([]byte{}, data...), data...)} {
		if verifyTar(bytes.NewReader(corrupt), m.record, "tar.gz") == nil {
			t.Fatal("corrupt/trailing gzip accepted")
		}
	}
}

func TestZIPRejectsTraversalAndSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{{"../escape", 0644}, {"relay.exe", os.ModeSymlink | 0777}, {"LICENSE", os.ModeNamedPipe | 0644}} {
		t.Run(fmt.Sprint(tc.name, tc.mode), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "bad.zip")
			f, e := os.Create(file)
			if e != nil {
				t.Fatal(e)
			}
			zw := zip.NewWriter(f)
			h := &zip.FileHeader{Name: tc.name, Method: zip.Store}
			h.SetMode(tc.mode)
			w, e := zw.CreateHeader(h)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = w.Write([]byte("target")); e != nil {
				t.Fatal(e)
			}
			if e = zw.Close(); e != nil {
				t.Fatal(e)
			}
			if e = f.Close(); e != nil {
				t.Fatal(e)
			}
			r := testRecord
			r.Target = "windows/amd64"
			if verifyArchive(file, "zip", r) == nil {
				t.Fatal("unsafe zip accepted")
			}
		})
	}
}

func TestDEBRejectsCorruptMemberHeadersAndTrailingContent(t *testing.T) {
	dir, m := fixtureRelease(t, "linux/amd64")
	var name string
	for _, a := range m.Artifacts {
		if a.Format == "deb" {
			name = a.Name
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"member name", func(b []byte) []byte { copy(b[8:24], "../escape/      "); return b }},
		{"timestamp", func(b []byte) []byte { b[8+16] = 'x'; return b }},
		{"ownership", func(b []byte) []byte { b[8+28] = '1'; return b }},
		{"mode", func(b []byte) []byte { copy(b[8+40:8+43], "777"); return b }},
		{"length", func(b []byte) []byte { copy(b[8+48:8+58], "9999999999"); return b }},
		{"truncated", func(b []byte) []byte { return b[:len(b)-3] }},
		{"trailing", func(b []byte) []byte { return append(b, 'x') }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := test.mutate(append([]byte(nil), data...))
			file := filepath.Join(t.TempDir(), "bad.deb")
			mustWrite(t, file, bad)
			if verifyDEB(file, m.record) == nil {
				t.Fatal("corrupt deb accepted")
			}
		})
	}
}

func TestZIPRejectsLocalTraversalAndTrailingBytes(t *testing.T) {
	dir, m := fixtureRelease(t, "windows/amd64")
	data, err := os.ReadFile(filepath.Join(dir, m.Artifacts[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"local header traversal", func(b []byte) []byte { copy(b[30:37], "../evil"); return b }},
		{"trailing content", func(b []byte) []byte { return append(b, []byte("trailing")...) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "bad.zip")
			mustWrite(t, file, test.mutate(append([]byte(nil), data...)))
			if verifyArchive(file, "zip", m.record) == nil {
				t.Fatal("ambiguous zip accepted")
			}
		})
	}
}

func TestStrictJSONRejectsDuplicateUnknownTrailingAndHugeMetadata(t *testing.T) {
	for _, data := range [][]byte{[]byte(`{"version":"0.1.0","version":"2.0.0"}`), []byte(`{"unknown":true}`), []byte(`{} {}`), bytes.Repeat([]byte(" "), int(maxMetadataBytes)+1), []byte(strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34))} {
		var r record
		if strictJSON(data, &r) == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}

func FuzzArchivePath(f *testing.F) {
	for _, s := range []string{"relay/relay", "../escape", "/absolute", "a\\b", "C:/escape", "", "relay/LICENSE"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		unsafeComponent := false
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				unsafeComponent = true
			}
		}
		if safeArchiveName(name) && (strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || unsafeComponent) {
			t.Fatalf("unsafe archive path accepted %q", name)
		}
	})
}
