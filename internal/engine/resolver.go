package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	k4allv1alpha1 "github.com/gpillon/k4all-operator/api/v1alpha1"
)

// VersionResolver resolves "latest" version strings by fetching versionUrl endpoints.
type VersionResolver struct {
	log    logr.Logger
	client *http.Client

	mu    sync.RWMutex
	cache map[string]cachedVersion
}

type cachedVersion struct {
	version   string
	fetchedAt time.Time
}

const versionCacheTTL = 5 * time.Minute

func NewVersionResolver(log logr.Logger) *VersionResolver {
	return &VersionResolver{
		log: log,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cache: make(map[string]cachedVersion),
	}
}

// Resolve returns the concrete version string for a component.
// If spec.Version is not "latest" (or empty), it is returned as-is.
// Otherwise the version is fetched from spec.VersionURL.
func (r *VersionResolver) Resolve(ctx context.Context, spec k4allv1alpha1.ComponentSpec) (string, error) {
	version := strings.TrimSpace(spec.Version)
	if version != "" && version != "latest" {
		return version, nil
	}
	if spec.VersionURL == "" {
		return version, nil
	}

	r.mu.RLock()
	cached, ok := r.cache[spec.VersionURL]
	r.mu.RUnlock()
	if ok && time.Since(cached.fetchedAt) < versionCacheTTL {
		return cached.version, nil
	}

	resolved, err := r.fetchVersion(ctx, spec.VersionURL)
	if err != nil {
		return "", fmt.Errorf("resolve version from %s: %w", spec.VersionURL, err)
	}

	r.mu.Lock()
	r.cache[spec.VersionURL] = cachedVersion{version: resolved, fetchedAt: time.Now()}
	r.mu.Unlock()

	return resolved, nil
}

func (r *VersionResolver) fetchVersion(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Handle GitHub redirect (releases/latest -> releases/tag/vX.Y.Z)
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		location := resp.Header.Get("Location")
		if idx := strings.LastIndex(location, "/"); idx >= 0 {
			return location[idx+1:], nil
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	version := strings.TrimSpace(string(body))

	// GitHub API returns JSON array for releases endpoints
	if strings.HasPrefix(version, "[") {
		return r.parseGitHubReleasesJSON(body)
	}

	// GitHub API returns JSON object for single release
	if strings.HasPrefix(version, "{") {
		return r.parseGitHubReleaseJSON(body)
	}

	// Plain text version string
	return version, nil
}

func (r *VersionResolver) parseGitHubReleasesJSON(data []byte) (string, error) {
	var releases []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
	}
	if err := json.Unmarshal(data, &releases); err != nil {
		return "", fmt.Errorf("parse GitHub releases JSON: %w", err)
	}
	for _, rel := range releases {
		if !rel.Prerelease && !rel.Draft {
			return rel.TagName, nil
		}
	}
	return "", fmt.Errorf("no stable release found in GitHub releases response")
}

func (r *VersionResolver) parseGitHubReleaseJSON(data []byte) (string, error) {
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return "", fmt.Errorf("parse GitHub release JSON: %w", err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("empty tag_name in GitHub release response")
	}
	return release.TagName, nil
}
