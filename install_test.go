package agentwire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		{Claude, InstallNPM, "@anthropic-ai/claude-code", "claude",
			[]string{"npm", "install", "-g", "@anthropic-ai/claude-code@latest"},
			[]string{"claude", "update"},
			[]string{"npm", "install", "-g", "@anthropic-ai/claude-code@2.1.0"}},
		{Codex, InstallNPM, "@openai/codex", "codex",
			[]string{"npm", "install", "-g", "@openai/codex@latest"},
			[]string{"codex", "update"},
			[]string{"npm", "install", "-g", "@openai/codex@2.1.0"}},
		{OpenCode, InstallNPM, "opencode-ai", "opencode",
			[]string{"npm", "install", "-g", "opencode-ai@latest"},
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
		if r.Harness != c.h || r.Method != c.method || r.Package != c.pkg || r.Bin != c.bin || len(r.Requires) == 0 {
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
	r.Requires[0] = "x"
	again, _ := Recipe(Claude)
	if again.UpdateOffEnv["DISABLE_UPDATES"] != "1" || again.Requires[0] != "npm" {
		t.Fatal("Recipe leaks the shared table")
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
		case "/-/package/@anthropic-ai/claude-code/dist-tags":
			_, _ = w.Write([]byte(`{"latest":"2.1.308","next":"2.2.0-beta.1"}`))
		case "/-/package/@openai/codex/dist-tags":
			_, _ = w.Write([]byte(`{"latest":"v0.130.0"}`))
		case "/-/package/opencode-ai/dist-tags":
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
		{Claude, "2.1.308", ""},
		{Codex, "0.130.0", ""},
		{OpenCode, "", "no version"},
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
