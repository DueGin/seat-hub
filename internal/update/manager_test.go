package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBinary answers `version` like a real build would.
func fakeBinary(version string) string {
	return fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then echo '{\"version\":\"%s\"}'; fi\n", version)
}

type fakeRelease struct {
	tag    string
	binary string
	sums   string // override; "" = correct checksums
	noSum  bool
}

type fakeGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	releases []fakeRelease
	gate     chan struct{} // when set, asset downloads block until closed
	calls    atomic.Int32
}

func newFakeGitHub(t *testing.T, releases ...fakeRelease) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{releases: releases}
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	gh.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.mu.Lock()
		releases, gate := gh.releases, gh.gate
		gh.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/DueGin/seat-hub/releases":
			gh.calls.Add(1)
			items := []map[string]any{}
			for _, release := range releases {
				assets := []map[string]any{{"name": asset, "browser_download_url": gh.URL + "/dl/" + release.tag + "/" + asset}}
				if !release.noSum {
					assets = append(assets, map[string]any{"name": ChecksumsAsset, "browser_download_url": gh.URL + "/dl/" + release.tag + "/" + ChecksumsAsset})
				}
				items = append(items, map[string]any{"tag_name": release.tag, "name": release.tag, "body": "- 更新 " + release.tag, "html_url": "https://example.test/" + release.tag, "published_at": "2026-11-01T00:00:00Z", "assets": assets})
			}
			items = append(items, map[string]any{"tag_name": "v9.9.9", "draft": true}, map[string]any{"tag_name": "v9.0.0-rc.1", "prerelease": true})
			_ = json.NewEncoder(w).Encode(items)
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")
			for _, release := range releases {
				if release.tag != parts[0] {
					continue
				}
				if parts[1] == ChecksumsAsset {
					sums := release.sums
					if sums == "" {
						hash := sha256.Sum256([]byte(release.binary))
						sums = hex.EncodeToString(hash[:]) + "  " + asset + "\n" + strings.Repeat("a", 64) + "  other_file\n"
					}
					io.WriteString(w, sums)
					return
				}
				if gate != nil {
					<-gate
				}
				io.WriteString(w, release.binary)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	return gh
}

type harness struct {
	m        *Manager
	l        layout
	restarts atomic.Int32
}

func newManager(t *testing.T, gh *fakeGitHub, current, supervisor string) *harness {
	t.Helper()
	h := &harness{l: layout{dir: t.TempDir()}}
	h.m = New(Config{
		Repo: "DueGin/seat-hub", APIBase: gh.URL, Dir: h.l.dir, Current: current,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Supervisor: supervisor,
		Restart: func() { h.restarts.Add(1) }, Now: func() time.Time { return clock },
	})
	return h
}

func (h *harness) wait(t *testing.T) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if job := h.m.Status(context.Background(), false).Job; job != nil && (job.Stage == StageRestarting || job.Stage == StageFailed) {
			return *job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

func TestStatusReportsWhyUpdatesAreUnavailable(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0")})
	for _, testcase := range []struct{ current, supervisor, reason string }{
		{"dev", "docker", "开发版本"},
		{"v1.0.0", "", "托管"},
	} {
		status := newManager(t, gh, testcase.current, testcase.supervisor).m.Status(context.Background(), true)
		if status.CanUpdate || !strings.Contains(status.Reason, testcase.reason) {
			t.Errorf("%+v: can=%v reason=%q", testcase, status.CanUpdate, status.Reason)
		}
		if _, err := newManager(t, gh, testcase.current, testcase.supervisor).m.Apply(context.Background(), "v1.1.0"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%+v: apply err=%v", testcase, err)
		}
	}
}

func TestStatusListsNewerReleasesAndCaches(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.2.0", binary: fakeBinary("v1.2.0")}, fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0")}, fakeRelease{tag: "v1.0.0", binary: fakeBinary("v1.0.0")})
	h := newManager(t, gh, "v1.0.0", "docker")
	status := h.m.Status(context.Background(), false)
	if !status.CanUpdate || !status.UpdateAvailable || status.Latest.Version != "v1.2.0" || len(status.Changelog) != 2 || status.Changelog[1].Version != "v1.1.0" {
		t.Fatalf("status=%+v", status)
	}
	h.m.Status(context.Background(), false)
	h.m.Status(context.Background(), true) // forced refresh is throttled
	if calls := gh.calls.Load(); calls != 1 {
		t.Fatalf("GitHub called %d times", calls)
	}
}

func TestApplyInstallsVerifiedBinaryAndRestarts(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0")})
	h := newManager(t, gh, "v1.0.0", "docker")
	if _, err := h.m.Apply(context.Background(), "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if job := h.wait(t); job.Stage != StageRestarting || job.Done == 0 {
		t.Fatalf("job=%+v", job)
	}
	if h.restarts.Load() != 1 {
		t.Fatal("restart not requested")
	}
	state := load(t, h.l)
	if state.Status != StatusPending || state.Current.Version != "v1.1.0" || state.Previous.Source != SourceImage || state.Previous.Version != "v1.0.0" || !intact(h.l.current(), state.Current.SHA256) {
		t.Fatalf("state=%+v", state)
	}
	// The launcher in the deployed v1.0.0 binary now picks the download.
	if path, err := Resolve(h.l.dir, "v1.0.0", clock); err != nil || path != h.l.current() {
		t.Fatalf("resolve path=%q err=%v", path, err)
	}
}

func TestApplyFromARunningDownloadKeepsItForRollback(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.2.0", binary: fakeBinary("v1.2.0")})
	h := newManager(t, gh, "v1.1.0", "systemd")
	running := install(t, h.l.current(), fakeBinary("v1.1.0"))
	save(t, h.l, State{Status: StatusActive, Current: &Installed{Version: "v1.1.0", SHA256: running, Source: SourceDownload}, Previous: &Installed{Version: "v1.0.0", Source: SourceImage}})
	if _, err := h.m.Apply(context.Background(), "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if job := h.wait(t); job.Stage != StageRestarting {
		t.Fatalf("job=%+v", job)
	}
	state := load(t, h.l)
	if state.Previous == nil || state.Previous.Version != "v1.1.0" || state.Previous.Source != SourceDownload || !intact(h.l.previous(), running) {
		t.Fatalf("previous=%+v", state.Previous)
	}
}

func TestApplyRejectsBadDownloads(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		release fakeRelease
		want    string
	}{
		{"checksum mismatch", fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0"), sums: strings.Repeat("0", 64) + "  " + AssetName(runtime.GOOS, runtime.GOARCH) + "\n"}, "SHA-256"},
		{"not in checksums", fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0"), sums: strings.Repeat("0", 64) + "  something_else\n"}, "校验文件中没有"},
		{"wrong self-reported version", fakeRelease{tag: "v1.1.0", binary: fakeBinary("v6.6.6")}, "不一致"},
		{"does not run", fakeRelease{tag: "v1.1.0", binary: "not an executable"}, "无法在当前系统运行"},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			gh := newFakeGitHub(t, testcase.release)
			h := newManager(t, gh, "v1.0.0", "docker")
			if _, err := h.m.Apply(context.Background(), "v1.1.0"); err != nil {
				t.Fatal(err)
			}
			job := h.wait(t)
			if job.Stage != StageFailed || !strings.Contains(job.Error, testcase.want) {
				t.Fatalf("job=%+v", job)
			}
			if h.restarts.Load() != 0 {
				t.Fatal("restarted after a failed download")
			}
			if state := load(t, h.l); state.Current != nil {
				t.Fatalf("installed after failure: %+v", state)
			}
			entries, _ := os.ReadDir(h.l.dir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".download-") {
					t.Fatalf("temporary file left behind: %s", entry.Name())
				}
			}
			// A failed job does not block the next attempt.
			if _, err := h.m.Apply(context.Background(), "v1.1.0"); err != nil {
				t.Fatalf("retry after failure: %v", err)
			}
			h.wait(t)
		})
	}
}

func TestApplyValidatesTheRequestedVersion(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.2.0", binary: fakeBinary("v1.2.0")}, fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0")}, fakeRelease{tag: "v1.0.0", binary: fakeBinary("v1.0.0")}, fakeRelease{tag: "v0.9.0", binary: fakeBinary("v0.9.0")})
	h := newManager(t, gh, "v1.0.0", "docker")
	for version, want := range map[string]error{"v1.1.0": ErrNotLatest, "v0.9.0": ErrNotNewer, "v1.0.0": ErrNotNewer, "../v1.2.0": ErrInvalidVersion, "v9.9.9": ErrNotLatest, "v9.0.0-rc.1": ErrNotLatest} {
		if _, err := h.m.Apply(context.Background(), version); !errors.Is(err, want) {
			t.Errorf("%s: err=%v want %v", version, err, want)
		}
	}
}

func TestApplyIsExclusive(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.1.0", binary: fakeBinary("v1.1.0")})
	gh.gate = make(chan struct{})
	h := newManager(t, gh, "v1.0.0", "docker")
	if _, err := h.m.Apply(context.Background(), "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Apply(context.Background(), "v1.1.0"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second apply err=%v", err)
	}
	if _, err := h.m.Rollback(context.Background()); !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNoRollback) {
		t.Fatalf("rollback during update err=%v", err)
	}
	close(gh.gate)
	h.wait(t)
}

func TestApplyWaitsForPendingConfirmation(t *testing.T) {
	gh := newFakeGitHub(t, fakeRelease{tag: "v1.2.0", binary: fakeBinary("v1.2.0")})
	h := newManager(t, gh, "v1.1.0", "docker")
	sum := install(t, h.l.current(), fakeBinary("v1.1.0"))
	save(t, h.l, State{Status: StatusPending, Current: &Installed{Version: "v1.1.0", SHA256: sum, Source: SourceDownload}, Previous: &Installed{Version: "v1.0.0", Source: SourceImage}})
	if _, err := h.m.Apply(context.Background(), "v1.2.0"); !errors.Is(err, ErrPending) {
		t.Fatalf("err=%v", err)
	}
	if status := h.m.Status(context.Background(), false); !status.Pending {
		t.Fatal("status does not report pending")
	}
}

func TestRollbackToImage(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newManager(t, gh, "v1.1.0", "docker")
	sum := install(t, h.l.current(), fakeBinary("v1.1.0"))
	save(t, h.l, State{Status: StatusActive, Current: &Installed{Version: "v1.1.0", SHA256: sum, Source: SourceDownload}, Previous: &Installed{Version: "v1.0.0", Source: SourceImage}})
	if status := h.m.Status(context.Background(), false); status.Rollback == nil || status.Rollback.Version != "v1.0.0" {
		t.Fatalf("rollback offer=%+v", status.Rollback)
	}
	if _, err := h.m.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRestart(t, h)
	if state := load(t, h.l); state.Current != nil || state.Previous != nil {
		t.Fatalf("state=%+v", state)
	}
	if path, _ := Resolve(h.l.dir, "v1.0.0", clock); path != "" {
		t.Fatalf("still launching %q", path)
	}
}

func TestRollbackToPreviousDownload(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newManager(t, gh, "v1.2.0", "docker")
	previous := install(t, h.l.previous(), fakeBinary("v1.1.0"))
	current := install(t, h.l.current(), fakeBinary("v1.2.0"))
	save(t, h.l, State{Status: StatusActive, Current: &Installed{Version: "v1.2.0", SHA256: current, Source: SourceDownload}, Previous: &Installed{Version: "v1.1.0", SHA256: previous, Source: SourceDownload}})
	if _, err := h.m.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRestart(t, h)
	state := load(t, h.l)
	if state.Current == nil || state.Current.Version != "v1.1.0" || state.Status != StatusActive || !intact(h.l.current(), previous) {
		t.Fatalf("state=%+v", state)
	}
	if _, err := h.m.Rollback(context.Background()); !errors.Is(err, ErrNoRollback) && !errors.Is(err, ErrBusy) {
		t.Fatalf("second rollback err=%v", err)
	}
}

func TestRollbackNeedsARunningDownload(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newManager(t, gh, "v1.0.0", "docker")
	if _, err := h.m.Rollback(context.Background()); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("err=%v", err)
	}
}

func waitRestart(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.restarts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("restart not requested")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUpstreamFailureIsReported(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.Close()
	h := newManager(t, gh, "v1.0.0", "docker")
	status := h.m.Status(context.Background(), true)
	if status.CheckError == "" || !strings.Contains(status.CheckError, "GitHub") {
		t.Fatalf("status=%+v", status)
	}
	if _, err := h.m.Apply(context.Background(), "v1.1.0"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err=%v", err)
	}
}
