package agentwire

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
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
	return r
}

var installRecipes = map[Harness]InstallRecipe{
	Claude: {Method: InstallNPM, Package: "@anthropic-ai/claude-code", Bin: "claude", Requires: []string{"npm"},
		Update:       []string{"update"},
		UpdateOffEnv: map[string]string{"DISABLE_AUTOUPDATER": "1", "DISABLE_UPDATES": "1"}},
	Codex: {Method: InstallNPM, Package: "@openai/codex", Bin: "codex", Requires: []string{"npm"},
		Update:        []string{"update"},
		UpdateOffArgs: []string{"-c", "check_for_update_on_startup=false"}},
	OpenCode: {Method: InstallNPM, Package: "opencode-ai", Bin: "opencode", Requires: []string{"npm"},
		Update: []string{"upgrade"}, UpdateTo: []string{"upgrade"},
		UpdateOffEnv: map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "1"}},
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

// LatestVersion asks the vendor registry for the newest release of h, such
// as "2.1.308". It uses Options.HTTPClient. A harness with no registry
// gives ErrUnsupported.
func (rt *Runtime) LatestVersion(ctx context.Context, h Harness) (string, error) {
	r, ok := Recipe(h)
	if !ok || r.Method != InstallNPM {
		return "", fmt.Errorf("%w: no release registry for harness %s", ErrUnsupported, h)
	}
	client := rt.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: latestTimeout}
	}
	url := npmRegistry + "/-/package/" + r.Package + "/dist-tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var tags struct {
		Latest string `json:"latest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	v := strings.TrimPrefix(strings.TrimSpace(tags.Latest), "v")
	if _, _, _, ok := parseSemver(v); !ok {
		return "", fmt.Errorf("GET %s: no version in %q", url, tags.Latest)
	}
	return v, nil
}
