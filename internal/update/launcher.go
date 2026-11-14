package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxAttempts is how many times a freshly installed version may fail to come
// up before the launcher gives up on it and rolls back.
const MaxAttempts = 3

// ChildEnv marks a process started by the launcher, so it does not launch again.
const ChildEnv = "SEAT_HUB_LAUNCHED"

// Dir is the update directory: PORTAL_UPDATE_DIR, or "app" next to the
// database (/data/app in the Docker image), so updates live on the same
// persistent volume as the data.
func Dir() string {
	if dir := strings.TrimSpace(os.Getenv("PORTAL_UPDATE_DIR")); dir != "" {
		return dir
	}
	db := strings.TrimSpace(os.Getenv("PORTAL_DB_PATH"))
	if db == "" {
		db = "./data/portal.db"
	}
	return filepath.Join(filepath.Dir(db), "app")
}

// Resolve decides, at process start, which binary should serve: it returns the
// path of a downloaded update to exec into, or "" to keep running the deployed
// binary (image). deployed is the version of the binary calling Resolve.
//
// A pending update is counted on every start; after MaxAttempts starts that
// never reached Confirm it is abandoned and the previous version restored.
func Resolve(dir, deployed string, now time.Time) (string, error) {
	l := layout{dir: dir}
	state, err := loadState(l)
	if err != nil {
		return "", err
	}
	if state.Current == nil {
		return "", nil
	}
	// The deployed binary is as new or newer (e.g. a newer image was pulled):
	// it wins, and the stale download is ignored.
	if !Newer(state.Current.Version, deployed) {
		return "", nil
	}
	if !intact(l.current(), state.Current.SHA256) {
		abandon(l, &state, deployed, "已下载的程序文件校验失败", now)
		if err := saveState(l, state, now); err != nil {
			return "", err
		}
		return chosen(l, state), nil
	}
	if state.Status == StatusPending {
		state.Attempts++
		if state.Attempts > MaxAttempts {
			abandon(l, &state, deployed, fmt.Sprintf("新版本连续 %d 次未能正常启动，已自动回滚", MaxAttempts), now)
		}
		if err := saveState(l, state, now); err != nil {
			return "", err
		}
	}
	return chosen(l, state), nil
}

func chosen(l layout, state State) string {
	if state.Current == nil {
		return ""
	}
	return l.current()
}

// abandon drops the current download and falls back to the previous version:
// a previous download if it is intact and newer than the deployed binary,
// otherwise the deployed binary itself. The fallback was running healthily
// before the update, so it is active straight away.
func abandon(l layout, state *State, deployed, reason string, now time.Time) {
	state.LastFailure = &Failure{Version: state.Current.Version, Error: reason, At: now.UTC().Format(time.RFC3339)}
	previous := state.Previous
	state.Current, state.Previous = nil, nil
	state.Status, state.Attempts = StatusActive, 0
	if previous != nil && previous.Source == SourceDownload && Newer(previous.Version, deployed) && intact(l.previous(), previous.SHA256) {
		if err := os.Rename(l.previous(), l.current()); err == nil {
			state.Current = previous
			return
		}
	}
	_ = os.Remove(l.current())
	_ = os.Remove(l.previous())
}

// NeedsConfirm reports whether version is a pending update that must prove
// itself (serve for a while) before it becomes the active version.
func NeedsConfirm(dir, version string) bool {
	state, err := loadState(layout{dir: dir})
	return err == nil && state.Status == StatusPending && state.Current != nil && state.Current.Version == version
}

// Confirm marks a pending update healthy. Only the version that is pending can
// confirm itself; anything else is a no-op.
func Confirm(dir, version string, now time.Time) error {
	l := layout{dir: dir}
	state, err := loadState(l)
	if err != nil {
		return err
	}
	if state.Status != StatusPending || state.Current == nil || state.Current.Version != version {
		return nil
	}
	state.Status, state.Attempts = StatusActive, 0
	return saveState(l, state, now)
}

// RestartExitCode is the exit status after installing an update. It is
// non-zero so supervisors restart the process whether their policy is
// "always" or "on-failure" (75 = EX_TEMPFAIL: try again).
const RestartExitCode = 75

// Timings for a freshly installed (pending) version to prove itself.
const (
	ConfirmAfter    = 30 * time.Second // serving this long counts as healthy
	ConfirmDeadline = 2 * time.Minute  // not serving by then counts as a failed start
)
