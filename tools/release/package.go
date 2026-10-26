package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type entry struct {
	name string
	data []byte
	mode int64
}

func assetStem(r record) string {
	return "relay_" + r.Version + "_" + strings.ReplaceAll(r.Target, "/", "_") + "_unsigned"
}

func packageRelease(root, binary, out string, r record) ([]artifact, error) {
	b, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxArtifactBytes {
		return nil, fmt.Errorf("binary is too large")
	}
	license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return nil, err
	}
	meta, _ := json.MarshalIndent(r, "", "  ")
	meta = append(meta, '\n')
	readme := []byte("Relay " + r.Version + "\nUNSIGNED DEVELOPMENT DISTRIBUTION\n\nVerify SHA256SUMS and manifest.json against a trusted source before use.\nNo automatic updater, installer or signature is included.\nLinux requires GTK3 and WebKitGTK 4.1 runtime libraries; Windows requires WebView2.\nThis archive never contains your Relay profile or credentials.\nSee docs/releases.md in the source tree for platform limitations.\n")
	stem := assetStem(r)
	common := []entry{{"LICENSE", license, 0644}, {"README.txt", readme, 0644}, {"RELEASE.json", meta, 0644}}
	switch strings.Split(r.Target, "/")[0] {
	case "windows":
		entries := append(common, entry{"relay.exe", b, 0755})
		name := stem + ".zip"
		if err = writeZIP(filepath.Join(out, name), entries, r.SourceDateEpoch); err != nil {
			return nil, err
		}
		return []artifact{{Name: name, Format: "zip"}}, nil
	case "darwin":
		version, _, _ := strings.Cut(r.Version, "-")
		plist := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" + `<plist version="1.0"><dict><key>CFBundleName</key><string>Relay</string><key>CFBundleDisplayName</key><string>Relay</string><key>CFBundleIdentifier</key><string>local.relay.desktop</string><key>CFBundleExecutable</key><string>relay</string><key>CFBundlePackageType</key><string>APPL</string><key>CFBundleShortVersionString</key><string>` + version + `</string><key>CFBundleVersion</key><string>` + version + `</string><key>NSHighResolutionCapable</key><true/><key>NSAppTransportSecurity</key><dict><key>NSAllowsArbitraryLoads</key><true/></dict></dict></plist>` + "\n")
		entries := []entry{{"Relay.app/Contents/MacOS/relay", b, 0755}, {"Relay.app/Contents/Info.plist", plist, 0644}, {"Relay.app/Contents/Resources/RELEASE.json", meta, 0644}, {"LICENSE", license, 0644}, {"README.txt", readme, 0644}}
		name := stem + ".tar.gz"
		if err = writeTarFile(filepath.Join(out, name), entries, r.SourceDateEpoch); err != nil {
			return nil, err
		}
		return []artifact{{Name: name, Format: "app-tar.gz"}}, nil
	case "linux":
		desktop, e := os.ReadFile(filepath.Join(root, "build", "relay.desktop"))
		if e != nil {
			return nil, e
		}
		icon, e := os.ReadFile(filepath.Join(root, "build", "relay.svg"))
		if e != nil {
			return nil, e
		}
		entries := append(common, entry{"relay", b, 0755}, entry{"relay.desktop", desktop, 0644}, entry{"relay.svg", icon, 0644})
		for i := range entries {
			entries[i].name = "relay/" + entries[i].name
		}
		name := stem + ".tar.gz"
		if err = writeTarFile(filepath.Join(out, name), entries, r.SourceDateEpoch); err != nil {
			return nil, err
		}
		arch := strings.Split(r.Target, "/")[1]
		debVersion := strings.ReplaceAll(r.Version, "-", "~")
		control := []byte("Package: relay\nVersion: " + debVersion + "\nSection: devel\nPriority: optional\nArchitecture: " + arch + "\nMaintainer: Relay contributors\nDepends: libgtk-3-0, libwebkit2gtk-4.1-0\nDescription: Local HTTP, gRPC and WebSocket API client\n Unsigned portable desktop API client. No automatic updater.\n")
		controlTar, e := tarBytes([]entry{{"control", control, 0644}}, r.SourceDateEpoch)
		if e != nil {
			return nil, e
		}
		dataTar, e := tarBytes([]entry{{"usr/bin/relay", b, 0755}, {"usr/share/applications/relay.desktop", desktop, 0644}, {"usr/share/icons/hicolor/scalable/apps/relay.svg", icon, 0644}, {"usr/share/doc/relay/copyright", license, 0644}, {"usr/share/doc/relay/README.txt", readme, 0644}, {"usr/share/doc/relay/RELEASE.json", meta, 0644}}, r.SourceDateEpoch)
		if e != nil {
			return nil, e
		}
		debName := stem + ".deb"
		if e = writeAR(filepath.Join(out, debName), []entry{{"debian-binary", []byte("2.0\n"), 0644}, {"control.tar.gz", controlTar, 0644}, {"data.tar.gz", dataTar, 0644}}, r.SourceDateEpoch); e != nil {
			return nil, e
		}
		return []artifact{{Name: name, Format: "tar.gz"}, {Name: debName, Format: "deb"}}, nil
	}
	return nil, fmt.Errorf("unsupported packaging target")
}

func sortedEntries(entries []entry) []entry {
	entries = append([]entry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries
}
func writeTar(w io.Writer, entries []entry, epoch int64) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Header.ModTime = time.Unix(epoch, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	for _, e := range sortedEntries(entries) {
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), ModTime: time.Unix(epoch, 0).UTC(), Typeflag: tar.TypeReg, Uid: 0, Gid: 0, Format: tar.FormatUSTAR}
		if err = tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err = tw.Write(e.data); err != nil {
			return err
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
func tarBytes(entries []entry, epoch int64) ([]byte, error) {
	var b bytes.Buffer
	err := writeTar(&b, entries, epoch)
	return b.Bytes(), err
}
func writeTarFile(name string, entries []entry, epoch int64) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	err = writeTar(f, entries, epoch)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func writeZIP(name string, entries []entry, epoch int64) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range sortedEntries(entries) {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetModTime(time.Unix(epoch, 0).UTC())
		h.SetMode(os.FileMode(e.mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err = w.Write(e.data); err != nil {
			return err
		}
	}
	if err = zw.Close(); err != nil {
		return err
	}
	return f.Close()
}
func writeAR(name string, entries []entry, epoch int64) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = io.WriteString(f, "!<arch>\n"); err != nil {
		return err
	}
	for _, e := range entries {
		h := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", e.name+"/", epoch, 0, 0, strconv.FormatInt(e.mode, 8), len(e.data))
		if len(h) != 60 {
			return fmt.Errorf("ar member header overflow")
		}
		if _, err = io.WriteString(f, h); err != nil {
			return err
		}
		if _, err = f.Write(e.data); err != nil {
			return err
		}
		if len(e.data)%2 != 0 {
			if _, err = f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	return f.Close()
}
