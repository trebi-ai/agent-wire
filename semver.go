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
// leading v, a pre-release suffix and trailing text.
func parseSemver(s string) (major, minor, patch int, ok bool) {
	s = strings.TrimPrefix(firstToken(s), "v")
	if i := strings.IndexAny(s, "-+"); i > 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	values := []*int{&major, &minor, &patch}
	for i, part := range parts {
		if i >= len(values) {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, 0, 0, false
		}
		*values[i] = n
	}
	return major, minor, patch, true
}
