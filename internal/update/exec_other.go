//go:build !unix

package update

import "log/slog"

// MaybeExec is a no-op where exec(2) is unavailable; in-site update is only
// supported on Unix hosts.
func MaybeExec(string, *slog.Logger) {}
