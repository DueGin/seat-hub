package update

import (
	"os"
	"testing"
	"time"
)

var clock = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// install writes a fake downloaded binary and returns its hash.
func install(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func save(t *testing.T, l layout, state State) {
	t.Helper()
	if err := saveState(l, state, clock); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, l layout) State {
	t.Helper()
	state, err := loadState(l)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestResolveRunsDeployedWithoutUpdate(t *testing.T) {
	path, err := Resolve(t.TempDir(), "v1.0.0", clock)
	if err != nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
}

func TestResolveRunsNewerDownload(t *testing.T) {
	l := layout{dir: t.TempDir()}
	sum := install(t, l.current(), "v1.1.0")
	save(t, l, State{Status: StatusActive, Current: &Installed{Version: "v1.1.0", SHA256: sum, Source: SourceDownload}})
	if path, err := Resolve(l.dir, "v1.0.0", clock); err != nil || path != l.current() {
		t.Fatalf("path=%q err=%v", path, err)
	}
	// A newer deployed binary (pulled image) wins over an older download.
	if path, err := Resolve(l.dir, "v1.2.0", clock); err != nil || path != "" {
		t.Fatalf("newer image: path=%q err=%v", path, err)
	}
}

func TestResolveCountsPendingAttemptsThenRollsBackToImage(t *testing.T) {
	l := layout{dir: t.TempDir()}
	sum := install(t, l.current(), "v1.1.0")
	save(t, l, State{Status: StatusPending, Current: &Installed{Version: "v1.1.0", SHA256: sum, Source: SourceDownload}, Previous: &Installed{Version: "v1.0.0", Source: SourceImage}})
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		path, err := Resolve(l.dir, "v1.0.0", clock)
		if err != nil || path != l.current() {
			t.Fatalf("attempt %d: path=%q err=%v", attempt, path, err)
		}
		if got := load(t, l).Attempts; got != attempt {
			t.Fatalf("attempts=%d want %d", got, attempt)
		}
	}
	path, err := Resolve(l.dir, "v1.0.0", clock)
	if err != nil || path != "" {
		t.Fatalf("after giving up: path=%q err=%v", path, err)
	}
	state := load(t, l)
	if state.Current != nil || state.Previous != nil || state.Status != StatusActive || state.LastFailure == nil || state.LastFailure.Version != "v1.1.0" {
		t.Fatalf("state=%+v", state)
	}
	if _, err := os.Stat(l.current()); !os.IsNotExist(err) {
		t.Fatal("abandoned binary was not removed")
	}
}

func TestResolveRollsBackToPreviousDownload(t *testing.T) {
	l := layout{dir: t.TempDir()}
	previousSum := install(t, l.previous(), "v1.1.0")
	currentSum := install(t, l.current(), "v1.2.0")
	save(t, l, State{
		Status: StatusPending, Attempts: MaxAttempts,
		Current:  &Installed{Version: "v1.2.0", SHA256: currentSum, Source: SourceDownload},
		Previous: &Installed{Version: "v1.1.0", SHA256: previousSum, Source: SourceDownload},
	})
	path, err := Resolve(l.dir, "v1.0.0", clock)
	if err != nil || path != l.current() {
		t.Fatalf("path=%q err=%v", path, err)
	}
	state := load(t, l)
	if state.Current == nil || state.Current.Version != "v1.1.0" || state.Status != StatusActive || state.LastFailure.Version != "v1.2.0" {
		t.Fatalf("state=%+v", state)
	}
	if !intact(l.current(), previousSum) {
		t.Fatal("previous binary not restored in place")
	}
}

func TestResolveAbandonsCorruptDownload(t *testing.T) {
	l := layout{dir: t.TempDir()}
	install(t, l.current(), "tampered")
	save(t, l, State{Status: StatusActive, Current: &Installed{Version: "v1.1.0", SHA256: "0000", Source: SourceDownload}})
	path, err := Resolve(l.dir, "v1.0.0", clock)
	if err != nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if state := load(t, l); state.Current != nil || state.LastFailure == nil {
		t.Fatalf("state=%+v", state)
	}
}

func TestConfirmOnlyThePendingVersion(t *testing.T) {
	l := layout{dir: t.TempDir()}
	sum := install(t, l.current(), "v1.1.0")
	save(t, l, State{Status: StatusPending, Attempts: 2, Current: &Installed{Version: "v1.1.0", SHA256: sum, Source: SourceDownload}})
	if !NeedsConfirm(l.dir, "v1.1.0") || NeedsConfirm(l.dir, "v1.0.0") {
		t.Fatal("NeedsConfirm")
	}
	if err := Confirm(l.dir, "v1.0.0", clock); err != nil {
		t.Fatal(err)
	}
	if load(t, l).Status != StatusPending {
		t.Fatal("other version confirmed the update")
	}
	if err := Confirm(l.dir, "v1.1.0", clock); err != nil {
		t.Fatal(err)
	}
	if state := load(t, l); state.Status != StatusActive || state.Attempts != 0 {
		t.Fatalf("state=%+v", state)
	}
}
