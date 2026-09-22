//go:build !windows

package proc

// envKey normalizes an environment key for duplicate detection. POSIX
// environments are case-sensitive.
func envKey(k string) string { return k }
