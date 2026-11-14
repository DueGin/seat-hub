package update

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Release is one published version.
type Release struct {
	Version     string            `json:"version"`
	Name        string            `json:"name"`
	Notes       string            `json:"notes"`
	URL         string            `json:"url"`
	PublishedAt time.Time         `json:"published_at"`
	assets      map[string]string // asset name -> download URL
}

// ChecksumsAsset lists "sha256  file" lines for every binary in a release.
const ChecksumsAsset = "checksums.txt"

// AssetName is the release binary for a platform, e.g. seat-hub_linux_amd64.
func AssetName(goos, goarch string) string { return BinaryName + "_" + goos + "_" + goarch }

var errUpstream = errors.New("release source unavailable")

// fetchReleases lists published, non-pre-release versions, newest first.
func (m *Manager) fetchReleases(ctx context.Context) ([]Release, error) {
	url := strings.TrimRight(m.cfg.APIBase, "/") + "/repos/" + m.cfg.Repo + "/releases?per_page=30"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	m.authorize(request)
	response, err := m.cfg.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstream, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GitHub 返回 %d", errUpstream, response.StatusCode)
	}
	var raw []struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: 无法解析版本列表", errUpstream)
	}
	releases := make([]Release, 0, len(raw))
	for _, item := range raw {
		if item.Draft || item.Prerelease || !ValidTag(item.TagName) {
			continue
		}
		release := Release{Version: item.TagName, Name: item.Name, Notes: item.Body, URL: item.HTMLURL, PublishedAt: item.PublishedAt, assets: map[string]string{}}
		for _, asset := range item.Assets {
			release.assets[asset.Name] = asset.URL
		}
		releases = append(releases, release)
	}
	sort.SliceStable(releases, func(i, j int) bool { return Newer(releases[i].Version, releases[j].Version) })
	return releases, nil
}

func (m *Manager) authorize(request *http.Request) {
	request.Header.Set("User-Agent", "seat-hub/"+m.cfg.Current)
	if m.cfg.Token != "" && strings.HasPrefix(request.URL.String(), strings.TrimRight(m.cfg.APIBase, "/")) {
		request.Header.Set("Authorization", "Bearer "+m.cfg.Token)
	}
}

// download streams url into w, failing past limit bytes. progress is told the
// running total and the size announced by the server (0 when unknown).
func (m *Manager) download(ctx context.Context, url string, w io.Writer, limit int64, progress func(done, total int64)) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	m.authorize(request)
	response, err := m.cfg.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", errUpstream, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: 下载返回 %d", errUpstream, response.StatusCode)
	}
	total := max(response.ContentLength, 0)
	if total > limit {
		return fmt.Errorf("文件过大（%d 字节）", total)
	}
	var done int64
	buffer := make([]byte, 64<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			done += int64(n)
			if done > limit {
				return fmt.Errorf("文件超过 %d 字节上限", limit)
			}
			if _, err := w.Write(buffer[:n]); err != nil {
				return err
			}
			if progress != nil {
				progress(done, total)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("%w: %v", errUpstream, readErr)
		}
	}
}

// parseChecksums reads sha256sum output ("<hex>  <name>" or "<hex> *<name>").
func parseChecksums(r io.Reader) map[string]string {
	sums := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return sums
}
