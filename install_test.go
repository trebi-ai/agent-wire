package agentwire

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestRecipe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		h       Harness
		method  InstallMethod
		pkg     string
		bin     string
		install []string
		update  []string
		pinned  []string
	}{
		{Claude, InstallBinary, "@anthropic-ai/claude-code", "claude", nil,
			[]string{"claude", "update"}, nil},
		{Codex, InstallBinary, "@openai/codex", "codex", nil,
			[]string{"codex", "update"}, nil},
		{OpenCode, InstallBinary, "opencode-ai", "opencode", nil,
			[]string{"opencode", "upgrade"},
			[]string{"opencode", "upgrade", "2.1.0"}},
		{Pi, InstallNPM, "@earendil-works/pi-coding-agent", "pi",
			[]string{"npm", "install", "-g", "@earendil-works/pi-coding-agent@latest"},
			[]string{"npm", "install", "-g", "@earendil-works/pi-coding-agent@latest"},
			[]string{"npm", "install", "-g", "@earendil-works/pi-coding-agent@2.1.0"}},
		{Copilot, InstallNPM, "@github/copilot", "copilot",
			[]string{"npm", "install", "-g", "@github/copilot@latest"}, nil, nil},
		{Gemini, InstallNPM, "@google/gemini-cli", "gemini",
			[]string{"npm", "install", "-g", "@google/gemini-cli@latest"}, nil, nil},
		{OMP, InstallNPM, "@oh-my-pi/pi-coding-agent", "omp",
			[]string{"npm", "install", "-g", "@oh-my-pi/pi-coding-agent@latest"}, nil, nil},
		{Grok, InstallNPM, "@xai-official/grok", "grok",
			[]string{"npm", "install", "-g", "@xai-official/grok@latest"},
			[]string{"grok", "update"},
			[]string{"grok", "update", "--version", "2.1.0"}},
		{Cursor, InstallScript, "https://cursor.com/install", "cursor-agent",
			[]string{"sh", "-c", "curl -fsSL https://cursor.com/install | bash"},
			[]string{"sh", "-c", "curl -fsSL https://cursor.com/install | bash"}, nil},
	}
	for _, c := range cases {
		r, ok := Recipe(c.h)
		if !ok {
			t.Errorf("%s: no recipe", c.h)
			continue
		}
		binary := c.method == InstallBinary
		if r.Harness != c.h || r.Method != c.method || r.Package != c.pkg || r.Bin != c.bin ||
			binary != (r.Download != nil) || binary != (len(r.Requires) == 0) {
			t.Errorf("%s: %+v", c.h, r)
		}
		if r.Bin != binName(c.h) {
			t.Errorf("%s: Bin %q, launch binary %q", c.h, r.Bin, binName(c.h))
		}
		if got := r.Command(""); !slices.Equal(got, c.install) {
			t.Errorf("%s: Command = %q, want %q", c.h, got, c.install)
		}
		if c.update != nil {
			if got := r.UpdateCommand(""); !slices.Equal(got, c.update) {
				t.Errorf("%s: UpdateCommand = %q, want %q", c.h, got, c.update)
			}
		}
		if binary && r.Command("") != nil {
			t.Errorf("%s: binary Command = %q, want nil", c.h, r.Command(""))
		}
		if c.pinned != nil {
			if got := r.UpdateCommand("2.1.0"); !slices.Equal(got, c.pinned) {
				t.Errorf("%s: UpdateCommand(2.1.0) = %q, want %q", c.h, got, c.pinned)
			}
		}
	}
	for _, h := range []Harness{OpenCode2, Fake, Native, "nope"} {
		if _, ok := Recipe(h); ok {
			t.Errorf("%s: want no recipe", h)
		}
	}
}

func TestRecipeUpdateOff(t *testing.T) {
	t.Parallel()
	cases := map[Harness]map[string]string{
		Claude:   {"DISABLE_AUTOUPDATER": "1", "DISABLE_UPDATES": "1"},
		OpenCode: {"OPENCODE_DISABLE_AUTOUPDATE": "1"},
		Pi:       {"PI_SKIP_VERSION_CHECK": "1"},
		Grok:     {"GROK_DISABLE_AUTOUPDATER": "1"},
		Copilot:  nil,
	}
	for h, want := range cases {
		r, _ := Recipe(h)
		if len(r.UpdateOffEnv) != len(want) {
			t.Errorf("%s: %v, want %v", h, r.UpdateOffEnv, want)
		}
		for k, v := range want {
			if r.UpdateOffEnv[k] != v {
				t.Errorf("%s: %s=%q, want %q", h, k, r.UpdateOffEnv[k], v)
			}
		}
	}
	codex, _ := Recipe(Codex)
	if !slices.Equal(codex.UpdateOffArgs, []string{"-c", "check_for_update_on_startup=false"}) {
		t.Errorf("codex args: %q", codex.UpdateOffArgs)
	}
}

// The launch of an isolated home sets the same update-off env as the recipe.
func TestRecipeUpdateOffMatchesLaunch(t *testing.T) {
	t.Parallel()
	for _, h := range []Harness{Claude, Pi} {
		l := &launch{env: map[string]string{}}
		applyHomeEnv(&StartRequest{Harness: h, Home: t.TempDir()}, l)
		r, _ := Recipe(h)
		for k, v := range r.UpdateOffEnv {
			if l.env[k] != v {
				t.Errorf("%s: launch %s=%q, recipe %q", h, k, l.env[k], v)
			}
		}
	}
}

func TestRecipeIsACopy(t *testing.T) {
	t.Parallel()
	r, _ := Recipe(Claude)
	r.UpdateOffEnv["DISABLE_UPDATES"] = "0"
	p, _ := Recipe(Pi)
	p.Requires[0] = "x"
	again, _ := Recipe(Claude)
	if again.UpdateOffEnv["DISABLE_UPDATES"] != "1" {
		t.Fatal("Recipe leaks the shared table")
	}
	if again, _ := Recipe(Pi); again.Requires[0] != "npm" {
		t.Fatal("Recipe leaks the shared table")
	}
	r.Download.Platforms["darwin/arm64"] = "x"
	r.Download.Archives = map[string]ArchiveKind{"darwin/arm64": ArchiveZip}
	again, _ = Recipe(Claude)
	if again.Download.Platforms["darwin/arm64"] != "darwin-arm64" || again.Download.Archives != nil {
		t.Fatal("Recipe leaks the shared download table")
	}
	c, _ := Recipe(Codex)
	c.Download.Programs["linux/amd64"] = "x"
	if again, _ := Recipe(Codex); again.Download.Programs["linux/amd64"] != "package/vendor/x86_64-unknown-linux-musl/bin/codex" {
		t.Fatal("Recipe leaks the shared program table")
	}
}

// registryRuntime sends every request to srv.
func registryRuntime(srv *httptest.Server) *Runtime {
	target, _ := url.Parse(srv.URL)
	client := srv.Client()
	base := client.Transport
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(req)
	})
	return New(Options{HTTPClient: client})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestLatestVersion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "accept", http.StatusBadRequest)
			return
		}
		switch r.URL.EscapedPath() {
		case "/-/package/@earendil-works/pi-coding-agent/dist-tags":
			_, _ = w.Write([]byte(`{"latest":"2.1.308","next":"2.2.0-beta.1"}`))
		case "/-/package/@xai-official/grok/dist-tags":
			_, _ = w.Write([]byte(`{"latest":"v0.130.0"}`))
		case "/-/package/@oh-my-pi/pi-coding-agent/dist-tags":
			_, _ = w.Write([]byte(`{"latest":"nightly"}`))
		case "/-/package/@github/copilot/dist-tags":
			_, _ = w.Write([]byte(`not json`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rt := registryRuntime(srv)
	cases := []struct {
		h       Harness
		want    string
		wantErr string
	}{
		{Pi, "2.1.308", ""},
		{Grok, "0.130.0", ""},
		{OMP, "", "no version"},
		{Copilot, "", "invalid character"},
		{Gemini, "", "404"},
	}
	for _, c := range cases {
		got, err := rt.LatestVersion(context.Background(), c.h)
		if got != c.want || (c.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: %q, %v; want %q, %q", c.h, got, err, c.want, c.wantErr)
		}
	}
	for _, h := range []Harness{Cursor, OpenCode2, Fake} {
		if _, err := rt.LatestVersion(context.Background(), h); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", h, err)
		}
	}
}

func TestLatestVersionContext(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registryRuntime(srv).LatestVersion(ctx, Claude); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// downloadServer fakes the vendor hosts of the binary recipes.
func downloadServer(t *testing.T, routes map[string]string) *Runtime {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return registryRuntime(srv)
}

const (
	hexA = "84590fefe6338be9087f5814b61f9a54c5f21524163a4c127cb5a808e31009f2"
	hexB = "8a232260df7a12c5e83e0b4b7659e1c8d9259b41942517bc0068d8ab55c36461"
)

func TestLatestVersionBinary(t *testing.T) {
	t.Parallel()
	rt := downloadServer(t, map[string]string{
		"/claude-code-releases/latest":       "2.1.9\n",
		"/-/package/@openai/codex/dist-tags": `{"latest":"0.9.0"}`,
		"/-/package/opencode-ai/dist-tags":   `{"latest":"nightly"}`,
	})
	cases := []struct {
		h       Harness
		want    string
		wantErr string
	}{
		{Claude, "2.1.9", ""},
		{Codex, "0.9.0", ""},
		{OpenCode, "", "no version"},
	}
	for _, c := range cases {
		got, err := rt.LatestVersion(context.Background(), c.h)
		if got != c.want || (c.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: %q, %v; want %q, %q", c.h, got, err, c.want, c.wantErr)
		}
	}
	html := downloadServer(t, map[string]string{"/claude-code-releases/latest": "<!doctype html><html></html>"})
	if _, err := html.LatestVersion(context.Background(), Claude); err == nil || !strings.Contains(err.Error(), "no version") {
		t.Errorf("html body: %v, want no version", err)
	}
}

func TestRecipeProgramPath(t *testing.T) {
	t.Parallel()
	codex := map[string]string{
		"darwin/arm64": "package/vendor/aarch64-apple-darwin/bin/codex",
		"darwin/amd64": "package/vendor/x86_64-apple-darwin/bin/codex",
		"linux/arm64":  "package/vendor/aarch64-unknown-linux-musl/bin/codex",
		"linux/amd64":  "package/vendor/x86_64-unknown-linux-musl/bin/codex",
	}[runtime.GOOS+"/"+runtime.GOARCH]
	want := map[Harness]string{
		Claude: "bin/claude", Codex: cmp.Or(codex, "bin/codex"), OpenCode: "package/bin/opencode",
		Pi: "bin/pi", Grok: "bin/grok", Cursor: "bin/cursor-agent",
	}
	for h, p := range want {
		r, _ := Recipe(h)
		if got := r.ProgramPath(); got != p {
			t.Errorf("%s: ProgramPath = %q, want %q", h, got, p)
		}
	}
}

func TestAssetManifest(t *testing.T) {
	t.Parallel()
	rt := downloadServer(t, map[string]string{
		"/claude-code-releases/latest": "2.1.9",
		"/claude-code-releases/2.1.9/manifest.json": `{"version":"2.1.9","platforms":{
			"darwin-arm64":{"binary":"claude","checksum":"` + hexA + `","size":347406976},
			"linux-x64":{"binary":"claude","checksum":"not hex","size":1}}}`,
	})
	ctx := context.Background()
	want := Asset{
		URL:    "https://downloads.claude.ai/claude-code-releases/2.1.9/darwin-arm64/claude",
		Digest: "sha256:" + hexA, Size: 347406976, Archive: ArchiveRaw, Program: "bin/claude",
	}
	for _, v := range []string{"2.1.9", "v2.1.9", ""} {
		got, err := rt.Asset(ctx, Claude, v, "darwin", "arm64")
		if err != nil || got != want {
			t.Errorf("version %q: %+v, %v; want %+v", v, got, err, want)
		}
	}
	if _, err := rt.Asset(ctx, Claude, "2.1.9", "darwin", "amd64"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("platform not in manifest: %v, want ErrUnsupported", err)
	}
	if _, err := rt.Asset(ctx, Claude, "2.1.9", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("bad checksum: %v, want digest error", err)
	}
	if _, err := rt.Asset(ctx, Claude, "9.9.9", "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown version: %v, want 404", err)
	}
	if _, err := rt.Asset(ctx, Claude, "../x", "darwin", "arm64"); err == nil {
		t.Error("unsafe version: want error")
	}
}

func TestAssetNPM(t *testing.T) {
	t.Parallel()
	sum512 := strings.Repeat("ab", 64)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 64))
	rt := downloadServer(t, map[string]string{
		"/-/package/@openai/codex/dist-tags": `{"latest":"0.9.0"}`,
		"/@openai/codex/0.9.0-darwin-arm64":  `{"dist":{"tarball":"https://example.test/codex.tgz","integrity":"` + integrity + `"}}`,
		"/@openai/codex/0.9.0-darwin-x64":    `{"dist":{"tarball":"https://example.test/codex-x.tgz","integrity":"sha1-abc"}}`,
		"/@openai/codex/0.9.0-linux-x64":     `{"dist":{"tarball":"http://example.test/codex.tgz","integrity":"` + integrity + `"}}`,
		"/opencode-linux-arm64/1.2.3":        `{"dist":{"tarball":"https://example.test/oc.tgz","integrity":"` + integrity + `"}}`,
	})
	ctx := context.Background()
	want := Asset{URL: "https://example.test/codex.tgz", Digest: "sha512:" + sum512, Archive: ArchiveTarGz,
		Program: "package/vendor/aarch64-apple-darwin/bin/codex"}
	for _, v := range []string{"0.9.0", ""} {
		got, err := rt.Asset(ctx, Codex, v, "darwin", "arm64")
		if err != nil || got != want {
			t.Errorf("codex %q: %+v, %v; want %+v", v, got, err, want)
		}
	}
	got, err := rt.Asset(ctx, OpenCode, "1.2.3", "linux", "arm64")
	want = Asset{URL: "https://example.test/oc.tgz", Digest: "sha512:" + sum512, Archive: ArchiveTarGz, Program: "package/bin/opencode"}
	if err != nil || got != want {
		t.Errorf("opencode: %+v, %v; want %+v", got, err, want)
	}
	if _, err := rt.Asset(ctx, Codex, "0.9.0", "darwin", "amd64"); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Errorf("sha1 integrity: %v, want integrity error", err)
	}
	if _, err := rt.Asset(ctx, Codex, "0.9.0", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "tarball") {
		t.Errorf("http tarball: %v, want tarball error", err)
	}
	if _, err := rt.Asset(ctx, Codex, "0.8.0", "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown version: %v, want 404", err)
	}
}

func TestAssetPlatform(t *testing.T) {
	t.Parallel()
	rt := New(Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no network expected")
	})}})
	ctx := context.Background()
	for _, h := range []Harness{Claude, Codex, OpenCode} {
		if _, err := rt.Asset(ctx, h, "1.0.0", "linux", "riscv64"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s riscv64: %v, want ErrUnsupported", h, err)
		}
	}
	for _, h := range []Harness{Pi, Cursor, Fake} {
		if _, err := rt.Asset(ctx, h, "1.0.0", "darwin", "arm64"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", h, err)
		}
	}
}
