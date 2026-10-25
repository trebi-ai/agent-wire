package agentwire

import (
	"strconv"
	"strings"
)

// semverAtLeast reports got >= want. An unparsable version is never at least.
func semverAtLeast(got, want string) bool {
	gmaj, gmin, gpatch, ok := parseSemver(got)
	if !ok {
		return false
	}
	wmaj, wmin, wpatch, ok := parseSemver(want)
	if !ok {
		return false
	}
	if gmaj != wmaj {
		return gmaj > wmaj
	}
	if gmin != wmin {
		return gmin > wmin
	}
	return gpatch >= wpatch
}

// parseSemver reads major.minor[.patch] from a version line, ignoring a
// leading v, a name prefix such as "grok " or "omp/", a pre-release suffix
// and trailing text.
func parseSemver(s string) (major, minor, patch int, ok bool) {
	for _, token := range strings.Fields(s) {
		major, minor, patch = 0, 0, 0
		token = strings.Trim(token, `"'()[],;`)
		if i := strings.LastIndexByte(token, '/'); i >= 0 {
			token = token[i+1:]
		}
		token = strings.TrimPrefix(token, "v")
		if i := strings.IndexAny(token, "-+"); i > 0 {
			token = token[:i]
		}
		parts := strings.Split(token, ".")
		if len(parts) < 2 {
			continue
		}
		values := []*int{&major, &minor, &patch}
		parsed := true
		for i, part := range parts {
			if i >= len(values) {
				break
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				parsed = false
				break
			}
			*values[i] = n
		}
		if parsed {
			return major, minor, patch, true
		}
	}
	return 0, 0, 0, false
}
