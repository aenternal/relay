// Release builds local, unsigned packages. It never publishes or installs them.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxArtifactBytes int64 = 512 << 20
const maxArchiveBytes int64 = 1 << 30
const maxMetadataBytes int64 = 1 << 20

type record struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	SourceDateEpoch int64  `json:"sourceDateEpoch"`
	Target          string `json:"target"`
	Unsigned        bool   `json:"unsigned"`
}
type artifact struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Schema  int    `json:"schema"`
	Product string `json:"product"`
	record
	Signing     string            `json:"signing"`
	SourceState string            `json:"sourceState"`
	Toolchains  map[string]string `json:"toolchains"`
	Limitations []string          `json:"limitations"`
	Artifacts   []artifact        `json:"artifacts"`
}
type config struct {
	root, out, version, commit, target, epoch, verify string
	skipFrontend                                      bool
}

func main() {
	var c config
	flag.StringVar(&c.root, "root", ".", "repository root")
	flag.StringVar(&c.out, "out", "", "new or empty output directory")
	flag.StringVar(&c.version, "version", "", "required expected SemVer, without v prefix")
	flag.StringVar(&c.commit, "commit", "", "full 40 or 64 lowercase hex source commit")
	flag.StringVar(&c.target, "target", runtime.GOOS+"/"+runtime.GOARCH, "linux/amd64, linux/arm64, windows/amd64, windows/arm64, darwin/amd64 or darwin/arm64")
	flag.StringVar(&c.epoch, "epoch", os.Getenv("SOURCE_DATE_EPOCH"), "fixed Unix timestamp; also SOURCE_DATE_EPOCH")
	flag.StringVar(&c.verify, "verify", "", "verify a local package directory; never execute its binaries")
	flag.BoolVar(&c.skipFrontend, "skip-frontend", false, "use the already built frontend (recorded in manifest)")
	flag.Parse()
	if flag.NArg() != 0 {
		fail(errors.New("unexpected positional argument"))
	}
	if c.verify != "" {
		if err := verifyDirectory(c.verify, c.version, c.commit); err != nil {
			fail(err)
		}
		fmt.Println("Verified checksums, archive structure and expected version; authenticity is UNSIGNED.")
		return
	}
	if err := buildRelease(c); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, "release:", err); os.Exit(1) }

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)
var commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func validateRecord(r record) error {
	if len(r.Version) > 80 || !versionPattern.MatchString(r.Version) {
		return errors.New("version must be a safe SemVer without v or build metadata")
	}
	if _, pre, ok := strings.Cut(r.Version, "-"); ok {
		for _, identifier := range strings.Split(pre, ".") {
			if len(identifier) > 1 && identifier[0] == '0' && strings.Trim(identifier, "0123456789") == "" {
				return errors.New("numeric prerelease identifiers must not have leading zeroes")
			}
		}
	}
	if !commitPattern.MatchString(r.Commit) {
		return errors.New("commit must be a full lowercase hexadecimal commit")
	}
	if r.SourceDateEpoch < 315532800 || r.SourceDateEpoch > 4294967295 {
		return errors.New("epoch must be between 1980-01-01 and 2106-02-07 (ZIP/gzip timestamp range)")
	}
	switch r.Target {
	case "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64", "darwin/amd64", "darwin/arm64":
	default:
		return errors.New("unsupported target")
	}
	if !r.Unsigned {
		return errors.New("this pipeline only supports explicitly unsigned packages")
	}
	return nil
}

func buildRelease(c config) error {
	epoch, err := strconv.ParseInt(c.epoch, 10, 64)
	if err != nil {
		return errors.New("a numeric --epoch or SOURCE_DATE_EPOCH is required")
	}
	r := record{c.version, c.commit, epoch, c.target, true}
	if err = validateRecord(r); err != nil {
		return err
	}
	parts := strings.Split(c.target, "/")
	if parts[0] != "windows" && (parts[0] != runtime.GOOS || parts[1] != runtime.GOARCH) {
		return errors.New("Linux/macOS native packages must be built on the matching OS and architecture")
	}
	if c.out == "" {
		return errors.New("--out must name a new or empty directory")
	}
	root, err := filepath.Abs(c.root)
	if err != nil {
		return err
	}
	gitHead := exec.Command("git", "rev-parse", "--verify", "HEAD")
	gitHead.Dir = root
	if head, e := gitHead.Output(); e == nil && strings.TrimSpace(string(head)) != r.Commit {
		return errors.New("--commit does not match the checked out base commit")
	}
	out, err := filepath.Abs(c.out)
	if err != nil {
		return err
	}
	if out == root || out == filepath.Join(root, "frontend", "dist") {
		return errors.New("output must be a separate directory")
	}
	if err = os.MkdirAll(out, 0755); err != nil {
		return err
	}
	outputInfo, err := os.Lstat(out)
	if err != nil {
		return err
	}
	if !outputInfo.IsDir() || outputInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("output must be a real directory, not a symlink")
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output directory must be empty; existing files are never overwritten")
	}
	if !c.skipFrontend {
		if err = command(filepath.Join(root, "frontend"), nil, "npm", "ci"); err != nil {
			return err
		}
		if err = command(filepath.Join(root, "frontend"), nil, "npm", "run", "build"); err != nil {
			return err
		}
	}
	index, err := os.ReadFile(filepath.Join(root, "frontend", "dist", "index.html"))
	if err != nil || len(index) == 0 {
		return errors.New("production frontend index.html is missing")
	}
	temp, err := os.MkdirTemp("", "relay-release-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	binary := filepath.Join(temp, "relay")
	if parts[0] == "windows" {
		binary += ".exe"
	}
	ldflags := "-s -w -buildid= -X relay/internal/buildinfo.Version=" + r.Version + " -X relay/internal/buildinfo.Commit=" + r.Commit + " -X relay/internal/buildinfo.SourceDateEpoch=" + strconv.FormatInt(epoch, 10) + " -X relay/internal/buildinfo.Unsigned=true"
	args := []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", binary}
	cgo := "1"
	if parts[0] == "linux" {
		args = append(args, "-tags", "production,gtk3")
	} else {
		args = append(args, "-tags", "production")
	}
	if parts[0] == "windows" {
		cgo = "0"
	}
	args = append(args, ".")
	// Go's -trimpath adds C prefix maps itself. Putting the absolute repository
	// path into CGO_CFLAGS would leak it back through Go's embedded build settings.
	env := []string{"GOOS=" + parts[0], "GOARCH=" + parts[1], "CGO_ENABLED=" + cgo, "SOURCE_DATE_EPOCH=" + strconv.FormatInt(epoch, 10), "TZ=UTC", "LC_ALL=C", "GOFLAGS=", "GOEXPERIMENT=", "CGO_CPPFLAGS=", "CGO_LDFLAGS=", "CGO_CFLAGS=-O2 -g0", "CGO_CXXFLAGS=-O2 -g0", "CC=cc", "CXX=c++"}
	if err = command(filepath.Join(root, "cmd", "desktop"), env, "go", args...); err != nil {
		return err
	}
	compiled, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	if bytes.Contains(compiled, []byte(root)) || bytes.Contains(compiled, []byte(temp)) {
		return errors.New("binary contains an absolute source or temporary build path")
	}
	tools := map[string]string{"go": toolVersion(root, "go", "version"), "node": toolVersion(root, "node", "--version"), "npm": toolVersion(root, "npm", "--version"), "frontend": "rebuilt from package-lock.json"}
	if c.skipFrontend {
		tools["frontend"] = "existing production dist; caller must verify its source"
	}
	if cgo == "1" {
		tools["cc"] = toolVersion(root, "cc", "--version")
	}
	if parts[0] == "linux" {
		tools["gtk"] = toolVersion(root, "pkg-config", "--modversion", "gtk+-3.0")
		tools["webkit"] = toolVersion(root, "pkg-config", "--modversion", "webkit2gtk-4.1")
	}
	sourceState := "working tree not authenticated by the base commit"
	status := exec.Command("git", "status", "--porcelain", "--untracked-files=normal")
	status.Dir = root
	if data, e := status.Output(); e == nil && len(bytes.TrimSpace(data)) == 0 {
		sourceState = "clean committed source"
	}
	m := manifest{Schema: 1, Product: "Relay", record: r, Signing: "UNSIGNED: no certificate, code signing, notarization or authenticated update feed", SourceState: sourceState, Toolchains: tools,
		Limitations: []string{"Checksums detect corruption, not publisher authenticity. Obtain the expected version and checksum through a trusted channel.", "Reproducibility requires identical source including frontend, toolchains, dependencies and system libraries.", "No installer is run, no automatic updates, no binaries are executed during verification."}}
	switch parts[0] {
	case "linux":
		m.Limitations = append(m.Limitations, "Native GTK3/WebKitGTK 4.1 libraries are required; portable tar does not bundle them. No AppImage/rpm is produced.")
	case "windows":
		m.Limitations = append(m.Limitations, "Portable Windows package requires Microsoft WebView2 Runtime; Windows runtime installation and UI must be checked on Windows.")
	case "darwin":
		m.Limitations = append(m.Limitations, "Unsigned .app tarball; Gatekeeper may block it. No signing or notarization. Native macOS runner required; no DMG is produced.")
	}
	assets, err := packageRelease(root, binary, out, r)
	if err != nil {
		return err
	}
	for _, a := range assets {
		info, e := os.Stat(filepath.Join(out, a.Name))
		if e != nil {
			return e
		}
		a.Size = info.Size()
		a.SHA256, e = fileHash(filepath.Join(out, a.Name))
		if e != nil {
			return e
		}
		m.Artifacts = append(m.Artifacts, a)
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Name < m.Artifacts[j].Name })
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err = os.WriteFile(filepath.Join(out, "manifest.json"), data, 0644); err != nil {
		return err
	}
	checks := []string{hexHash(data) + "  manifest.json"}
	for _, a := range m.Artifacts {
		checks = append(checks, a.SHA256+"  "+a.Name)
	}
	sort.Strings(checks)
	if err = os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(strings.Join(checks, "\n")+"\n"), 0644); err != nil {
		return err
	}
	for _, name := range append([]string{"manifest.json", "SHA256SUMS"}, artifactNames(m.Artifacts)...) {
		if err = os.Chtimes(filepath.Join(out, name), time.Unix(epoch, 0), time.Unix(epoch, 0)); err != nil {
			return err
		}
	}
	if err = verifyDirectory(out, r.Version, r.Commit); err != nil {
		return fmt.Errorf("self verification: %w", err)
	}
	fmt.Println("Unsigned packages verified:", out)
	return nil
}

func artifactNames(as []artifact) []string {
	var names []string
	for _, a := range as {
		names = append(names, a.Name)
	}
	return names
}
func hexHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func fileHash(name string) (string, error) {
	f, e := os.Open(name)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, maxArtifactBytes+1))
	if e != nil {
		return "", e
	}
	if n > maxArtifactBytes {
		return "", errors.New("artifact is too large")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func command(dir string, extra []string, name string, args ...string) error {
	cmd := toolCommand(name, args...)
	cmd.Dir = dir
	cmd.Env = mergedEnvironment(extra)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
func mergedEnvironment(extra []string) []string {
	values := map[string]string{}
	for _, s := range append(os.Environ(), extra...) {
		k, v, ok := strings.Cut(s, "=")
		if ok {
			values[k] = v
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}
func toolVersion(dir, name string, args ...string) string {
	cmd := toolCommand(name, args...)
	cmd.Dir = dir
	cmd.Env = mergedEnvironment([]string{"LC_ALL=C"})
	data, err := cmd.Output()
	if err != nil {
		return "unavailable"
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}

func toolCommand(name string, args ...string) *exec.Cmd {
	// npm is a .cmd launcher on Windows. Only these fixed, internal npm
	// invocations enter cmd.exe; user metadata never enters shell command text.
	if runtime.GOOS == "windows" && name == "npm" {
		line := strings.Join(args, " ")
		switch line {
		case "ci", "run build", "--version":
			return exec.Command("cmd.exe", "/d", "/s", "/c", "npm "+line)
		}
		return exec.Command("relay-invalid-npm-invocation")
	}
	return exec.Command(name, args...)
}

// strictJSON rejects duplicate object keys as well as unknown schema fields.
func strictJSON(data []byte, value any) error {
	if int64(len(data)) > maxMetadataBytes {
		return errors.New("metadata is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	budget := 10000
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("metadata nesting limit exceeded")
		}
		budget--
		if budget < 0 {
			return errors.New("metadata token limit exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return errors.New("duplicate or invalid metadata key")
				}
				seen[s] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid metadata nesting")
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing metadata")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
