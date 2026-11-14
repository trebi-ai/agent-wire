package agentwire

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// InstallMethod is how a harness binary gets on the machine.
type InstallMethod string

const (
	// InstallNPM installs a global npm package.
	InstallNPM InstallMethod = "npm"
	// InstallBrew installs a Homebrew formula.
	InstallBrew InstallMethod = "brew"
	// InstallScript pipes a vendor install script into a shell.
	InstallScript InstallMethod = "script"
	// InstallBinary downloads a binary. The caller does the download.
	InstallBinary InstallMethod = "binary"
)

// ArchiveKind is the file format of a binary download.
type ArchiveKind string

const (
	// ArchiveRaw is the program file itself.
	ArchiveRaw ArchiveKind = "raw"
	// ArchiveTarGz is a gzip tar archive.
	ArchiveTarGz ArchiveKind = "tar.gz"
	// ArchiveZip is a zip archive.
	ArchiveZip ArchiveKind = "zip"
)

// DownloadSource is where the release data of a binary recipe lives.
type DownloadSource string

const (
	// SourceManifest is a vendor host with a version file and a manifest
	// per version.
	SourceManifest DownloadSource = "manifest"
	// SourceNPM is the npm registry, read over HTTPS. The install needs no
	// npm program.
	SourceNPM DownloadSource = "npm"
)

// Download holds the vendor facts of a binary install.
type Download struct {
	Source DownloadSource
	// Base is the manifest base URL, or the npm package that gives the
	// latest version.
	Base string
	// Asset is the npm package of one platform, as "name@version".
	// {version} and {platform} are placeholders. The manifest source does
	// not use it.
	Asset string
	// Channel is the manifest file that holds the latest version.
	Channel string
	// Platforms maps "goos/goarch" to the platform name in the vendor
	// asset. A platform that is not here has no binary.
	Platforms map[string]string
	// Archive is the file format of the download.
	Archive ArchiveKind
	// Archives overrides Archive for the listed "goos/goarch" platforms.
	Archives map[string]ArchiveKind
	// Program is the path of the program in the unpacked folder.
	Program string
	// Programs overrides Program for the listed "goos/goarch" platforms.
	Programs map[string]string
}

// Asset is one downloadable release file. The caller downloads it, checks
// Digest and Size, and unpacks it.
type Asset struct {
	URL string
	// Digest is "sha256:<hex>" or "sha512:<hex>".
	Digest string
	// Size is the byte size. Zero means that the vendor gives none.
	Size    int64
	Archive ArchiveKind
	// Program is the path of the program in the unpacked folder.
	Program string
}

func (d Download) clone() *Download {
	d.Platforms = maps.Clone(d.Platforms)
	d.Archives = maps.Clone(d.Archives)
	d.Programs = maps.Clone(d.Programs)
	return &d
}

// program is the path of the program for one platform.
func (d Download) program(goos, goarch string) string {
	if p, ok := d.Programs[goos+"/"+goarch]; ok {
		return p
	}
	return d.Program
}

// InstallRecipe holds the vendor facts to install and update one harness.
// The library runs none of these commands.
type InstallRecipe struct {
	Harness Harness
	Method  InstallMethod
	// Package is the npm package, the brew formula, the script URL, or the
	// download URL.
	Package string
	// Bin is the binary that the install puts on PATH.
	Bin string
	// Requires lists the programs that must be on PATH before the install.
	Requires []string
	// Update is the vendor self-update command after Bin. Nil means install
	// again.
	Update []string
	// UpdateTo is the command after Bin that installs one version. The
	// version goes last. Nil means install again.
	UpdateTo []string
	// UpdateOffEnv is the env that stops the vendor auto-update.
	UpdateOffEnv map[string]string
	// UpdateOffArgs are launch args that stop the vendor auto-update.
	UpdateOffArgs []string
	// Download holds the facts of the binary method. Nil for other methods.
	Download *Download
}

// Command is the install command line. An empty version means the latest.
// The script method ignores the version. The binary method gives nil.
func (r InstallRecipe) Command(version string) []string {
	switch r.Method {
	case InstallNPM:
		if version == "" {
			version = "latest"
		}
		return []string{"npm", "install", "-g", r.Package + "@" + version}
	case InstallBrew:
		return []string{"brew", "install", r.Package}
	case InstallScript:
		return []string{"sh", "-c", "curl -fsSL " + r.Package + " | bash"}
	}
	return nil
}

// ProgramPath is the path of the program in the install folder of one
// version, on this machine.
func (r InstallRecipe) ProgramPath() string {
	if r.Download != nil {
		if p := r.Download.program(runtime.GOOS, runtime.GOARCH); p != "" {
			return p
		}
	}
	return "bin/" + r.Bin
}

// UpdateCommand is the command line that updates Bin. An empty version
// means the latest. It falls back to Command when the vendor has no
// matching self-update.
func (r InstallRecipe) UpdateCommand(version string) []string {
	switch {
	case version == "" && r.Update != nil:
		return append([]string{r.Bin}, r.Update...)
	case version != "" && r.UpdateTo != nil:
		return append(append([]string{r.Bin}, r.UpdateTo...), version)
	}
	return r.Command(version)
}

func (r InstallRecipe) clone() InstallRecipe {
	r.Requires = slices.Clone(r.Requires)
	r.Update = slices.Clone(r.Update)
	r.UpdateTo = slices.Clone(r.UpdateTo)
	r.UpdateOffEnv = maps.Clone(r.UpdateOffEnv)
	r.UpdateOffArgs = slices.Clone(r.UpdateOffArgs)
	if r.Download != nil {
		r.Download = r.Download.clone()
	}
	return r
}

var installRecipes = map[Harness]InstallRecipe{
	Claude: {Method: InstallBinary, Package: "@anthropic-ai/claude-code", Bin: "claude",
		Update:       []string{"update"},
		UpdateOffEnv: map[string]string{"DISABLE_AUTOUPDATER": "1", "DISABLE_UPDATES": "1"},
		Download: &Download{Source: SourceManifest, Base: "https://downloads.claude.ai/claude-code-releases",
			Channel: "latest", Archive: ArchiveRaw, Program: "bin/claude",
			Platforms: map[string]string{
				"darwin/arm64": "darwin-arm64", "darwin/amd64": "darwin-x64",
				"linux/arm64": "linux-arm64", "linux/amd64": "linux-x64"}}},
	// The npm package of each platform holds the vendor folder of the
	// release, with the program and its helper files.
	Codex: {Method: InstallBinary, Package: "@openai/codex", Bin: "codex",
		Update:        []string{"update"},
		UpdateOffArgs: []string{"-c", "check_for_update_on_startup=false"},
		Download: &Download{Source: SourceNPM, Base: "@openai/codex", Asset: "@openai/codex@{version}-{platform}",
			Archive: ArchiveTarGz,
			Platforms: map[string]string{
				"darwin/arm64": "darwin-arm64", "darwin/amd64": "darwin-x64",
				"linux/arm64": "linux-arm64", "linux/amd64": "linux-x64"},
			Programs: map[string]string{
				"darwin/arm64": "package/vendor/aarch64-apple-darwin/bin/codex",
				"darwin/amd64": "package/vendor/x86_64-apple-darwin/bin/codex",
				"linux/arm64":  "package/vendor/aarch64-unknown-linux-musl/bin/codex",
				"linux/amd64":  "package/vendor/x86_64-unknown-linux-musl/bin/codex"}}},
	OpenCode: {Method: InstallBinary, Package: "opencode-ai", Bin: "opencode",
		Update: []string{"upgrade"}, UpdateTo: []string{"upgrade"},
		UpdateOffEnv: map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "1"},
		Download: &Download{Source: SourceNPM, Base: "opencode-ai", Asset: "opencode-{platform}@{version}",
			Archive: ArchiveTarGz, Program: "package/bin/opencode",
			Platforms: map[string]string{
				"darwin/arm64": "darwin-arm64", "darwin/amd64": "darwin-x64",
				"linux/arm64": "linux-arm64", "linux/amd64": "linux-x64"}}},
	Pi: {Method: InstallNPM, Package: "@earendil-works/pi-coding-agent", Bin: "pi", Requires: []string{"npm"},
		UpdateOffEnv: map[string]string{"PI_SKIP_VERSION_CHECK": "1"}},
	Copilot: {Method: InstallNPM, Package: "@github/copilot", Bin: "copilot", Requires: []string{"npm"}},
	Gemini:  {Method: InstallNPM, Package: "@google/gemini-cli", Bin: "gemini", Requires: []string{"npm"}},
	// omp runs on bun.
	OMP: {Method: InstallNPM, Package: "@oh-my-pi/pi-coding-agent", Bin: "omp", Requires: []string{"npm", "bun"},
		UpdateOffEnv: map[string]string{"PI_SKIP_VERSION_CHECK": "1"}},
	// grok also reads "[cli] auto_update = false" in config.toml.
	Grok: {Method: InstallNPM, Package: "@xai-official/grok", Bin: "grok", Requires: []string{"npm"},
		Update: []string{"update"}, UpdateTo: []string{"update", "--version"},
		UpdateOffEnv: map[string]string{"GROK_DISABLE_AUTOUPDATER": "1"}},
	Cursor: {Method: InstallScript, Package: "https://cursor.com/install", Bin: "cursor-agent", Requires: []string{"curl", "bash"}},
}

// Recipe returns the install facts of h. ok=false means that the harness
// has no vendor install, such as the native and fake harnesses.
func Recipe(h Harness) (InstallRecipe, bool) {
	r, ok := installRecipes[h]
	if !ok {
		return InstallRecipe{}, false
	}
	r = r.clone()
	r.Harness = h
	return r, true
}

// npmRegistry is the npm registry base URL.
const npmRegistry = "https://registry.npmjs.org"

// latestTimeout bounds one release lookup.
const latestTimeout = 10 * time.Second

// versionRe is a release version: major.minor.patch with an optional
// pre-release or build suffix.
var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)

// sha256Re is a lowercase hex sha256 digest.
var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxMetaBytes bounds one metadata response.
const maxMetaBytes = 8 << 20

// get reads one metadata response. It uses Options.HTTPClient.
func (rt *Runtime) get(ctx context.Context, url, accept string) ([]byte, error) {
	client := rt.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: latestTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBytes))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return body, nil
}

// getJSON reads one JSON response into v.
func (rt *Runtime) getJSON(ctx context.Context, url string, v any) error {
	body, err := rt.get(ctx, url, "application/json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	return nil
}

// LatestVersion asks the vendor registry or release feed for the newest
// release of h, such as "2.1.308". It uses Options.HTTPClient. A harness
// with no such source gives ErrUnsupported.
func (rt *Runtime) LatestVersion(ctx context.Context, h Harness) (string, error) {
	r, ok := Recipe(h)
	if !ok || (r.Method != InstallNPM && r.Download == nil) {
		return "", fmt.Errorf("%w: no release registry for harness %s", ErrUnsupported, h)
	}
	if r.Download != nil {
		return rt.latestDownload(ctx, r.Download)
	}
	return rt.npmLatest(ctx, r.Package)
}

// npmLatest reads the "latest" dist-tag of one npm package.
func (rt *Runtime) npmLatest(ctx context.Context, pkg string) (string, error) {
	url := npmRegistry + "/-/package/" + pkg + "/dist-tags"
	var tags struct {
		Latest string `json:"latest"`
	}
	if err := rt.getJSON(ctx, url, &tags); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(strings.TrimSpace(tags.Latest), "v")
	if _, _, _, ok := parseSemver(v); !ok {
		return "", fmt.Errorf("GET %s: no version in %q", url, tags.Latest)
	}
	return v, nil
}

func (rt *Runtime) latestDownload(ctx context.Context, d *Download) (string, error) {
	switch d.Source {
	case SourceManifest:
		url := d.Base + "/" + d.Channel
		body, err := rt.get(ctx, url, "text/plain")
		if err != nil {
			return "", err
		}
		v := strings.TrimSpace(string(body))
		if !versionRe.MatchString(v) {
			return "", fmt.Errorf("GET %s: no version in %q", url, clip(v))
		}
		return v, nil
	case SourceNPM:
		return rt.npmLatest(ctx, d.Base)
	}
	return "", fmt.Errorf("%w: download source %q", ErrUnsupported, d.Source)
}

// clip shortens a vendor text for an error message.
func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// Asset resolves the download of version for one platform: the URL, the
// digest, the size, and the archive format. An empty version means
// the latest. It reads metadata only and never downloads the file. A harness
// with no binary download, or a platform that the vendor does not list,
// gives ErrUnsupported.
func (rt *Runtime) Asset(ctx context.Context, h Harness, version, goos, goarch string) (Asset, error) {
	r, ok := Recipe(h)
	if !ok || r.Download == nil {
		return Asset{}, fmt.Errorf("%w: no binary download for harness %s", ErrUnsupported, h)
	}
	d := r.Download
	platform, ok := d.Platforms[goos+"/"+goarch]
	if !ok {
		return Asset{}, fmt.Errorf("%w: no %s binary for %s/%s", ErrUnsupported, h, goos, goarch)
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		var err error
		if version, err = rt.latestDownload(ctx, d); err != nil {
			return Asset{}, err
		}
	}
	if !versionRe.MatchString(version) {
		return Asset{}, fmt.Errorf("invalid version %q", clip(version))
	}
	archive := d.Archive
	if k, ok := d.Archives[goos+"/"+goarch]; ok {
		archive = k
	}
	a := Asset{Archive: archive, Program: d.program(goos, goarch)}
	switch d.Source {
	case SourceManifest:
		return rt.manifestAsset(ctx, r, a, version, platform)
	case SourceNPM:
		return rt.npmAsset(ctx, d, a, version, platform)
	}
	return Asset{}, fmt.Errorf("%w: download source %q", ErrUnsupported, d.Source)
}

func (rt *Runtime) manifestAsset(ctx context.Context, r InstallRecipe, a Asset, version, platform string) (Asset, error) {
	d := r.Download
	url := d.Base + "/" + version + "/manifest.json"
	var m struct {
		Platforms map[string]struct {
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
		} `json:"platforms"`
	}
	if err := rt.getJSON(ctx, url, &m); err != nil {
		return Asset{}, err
	}
	p, ok := m.Platforms[platform]
	if !ok {
		return Asset{}, fmt.Errorf("%w: %s %s has no platform %s", ErrUnsupported, r.Harness, version, platform)
	}
	digest, err := normalizeDigest(p.Checksum)
	if err != nil {
		return Asset{}, fmt.Errorf("GET %s: %w", url, err)
	}
	a.URL = d.Base + "/" + version + "/" + platform + "/" + r.Bin
	a.Digest, a.Size = digest, p.Size
	return a, nil
}

// npmAsset reads the dist facts of the platform package from the npm
// registry. The size stays zero: the registry gives none for the tarball.
func (rt *Runtime) npmAsset(ctx context.Context, d *Download, a Asset, version, platform string) (Asset, error) {
	spec := strings.NewReplacer("{version}", version, "{platform}", platform).Replace(d.Asset)
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return Asset{}, fmt.Errorf("%w: bad npm spec %q", ErrUnsupported, spec)
	}
	url := npmRegistry + "/" + spec[:i] + "/" + spec[i+1:]
	var m struct {
		Dist struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if err := rt.getJSON(ctx, url, &m); err != nil {
		return Asset{}, err
	}
	if !strings.HasPrefix(m.Dist.Tarball, "https://") {
		return Asset{}, fmt.Errorf("GET %s: bad tarball URL %q", url, clip(m.Dist.Tarball))
	}
	digest, err := integrityDigest(m.Dist.Integrity)
	if err != nil {
		return Asset{}, fmt.Errorf("GET %s: %w", url, err)
	}
	a.URL, a.Digest = m.Dist.Tarball, digest
	return a, nil
}

// integrityDigest gives "sha512:<hex>" from an npm "sha512-<base64>"
// integrity value.
func integrityDigest(s string) (string, error) {
	b64, ok := strings.CutPrefix(strings.TrimSpace(s), "sha512-")
	sum, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(sum) != 64 {
		return "", fmt.Errorf("bad sha512 integrity %q", clip(s))
	}
	return "sha512:" + hex.EncodeToString(sum), nil
}

// normalizeDigest gives "sha256:<hex>" from "<hex>" or "sha256:<hex>".
func normalizeDigest(s string) (string, error) {
	sum := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "sha256:"))
	if !sha256Re.MatchString(sum) {
		return "", fmt.Errorf("bad sha256 digest %q", clip(s))
	}
	return "sha256:" + sum, nil
}
