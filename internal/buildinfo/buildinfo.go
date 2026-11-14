// Package buildinfo carries the version stamped in at build time:
//
//	go build -ldflags "-X github.com/DueGin/seat-hub/internal/buildinfo.Version=v1.2.3 \
//	  -X github.com/DueGin/seat-hub/internal/buildinfo.Commit=<sha> \
//	  -X github.com/DueGin/seat-hub/internal/buildinfo.BuildTime=<RFC3339>"
//
// An unstamped build reports "dev" and is never offered an in-site update.
package buildinfo

import "strings"

var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
)

// Info is the running build, as reported by `seat-hub version` and the API.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

func Current() Info {
	return Info{Version: strings.TrimSpace(Version), Commit: strings.TrimSpace(Commit), BuildTime: strings.TrimSpace(BuildTime)}
}

// IsRelease reports whether this is a stamped release build (vX.Y.Z...).
func (info Info) IsRelease() bool {
	return info.Version != "" && info.Version != "dev" && strings.HasPrefix(info.Version, "v")
}
