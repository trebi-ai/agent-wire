//go:build windows

package proc

import "strings"

// envKey normalizes an environment key for duplicate detection. Windows
// treats Path and PATH as the same variable, so an override must replace the
// inherited entry instead of adding a second one.
func envKey(k string) string { return strings.ToUpper(k) }
