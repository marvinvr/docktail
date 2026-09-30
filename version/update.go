package version

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// ReleasesURL is where the release notes live; the update notice points here.
const ReleasesURL = "https://github.com/marvinvr/docktail/releases"

// tagsURL lists the repository's git tags. Every tag is published as an image
// tag by the release workflow, and every stable one also moves :latest, so the
// highest stable tag is the newest image a user can pull.
const tagsURL = "https://api.github.com/repos/marvinvr/docktail/tags"

const (
	updateCheckDelay    = 30 * time.Second // first check, after startup has settled
	updateCheckInterval = 24 * time.Hour
	updateCheckRetry    = time.Hour // after a failed check
	updateCheckTimeout  = 15 * time.Second
	maxTagPages         = 10 // 100 tags a page
)

// RunUpdateCheck looks for a newer stable DockTail release shortly after
// startup and then once a day (hourly after a failure), and logs each newer
// version it finds once. Each check is an anonymous GET to the GitHub API that
// sends nothing about this installation. Builds that are not a release version (dev, latest) skip it,
// and every failure is logged at debug level only. Blocks until ctx ends.
func RunUpdateCheck(ctx context.Context, logger zerolog.Logger) {
	current, ok := Parse(Version)
	if !ok {
		logger.Debug().Str("version", Version).Msg("Update check skipped: not a release build")
		return
	}
	client := &http.Client{Timeout: updateCheckTimeout}
	var announced string
	wait := updateCheckDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		latest, err := latestStable(ctx, client)
		if err != nil {
			logger.Debug().Err(err).Msg("Update check failed")
			wait = updateCheckRetry
			continue
		}
		wait = updateCheckInterval
		if Compare(latest, current) <= 0 || latest.String() == announced {
			continue
		}
		announced = latest.String()
		logger.Info().
			Str("current", Version).
			Str("latest", announced).
			Str("release_notes", ReleasesURL).
			Msg("A newer DockTail release is available; pull the new image and recreate the container to update (set UPDATE_CHECK=false to stop checking)")
	}
}

// latestStable returns the highest stable semver among the repository's tags.
func latestStable(ctx context.Context, client *http.Client) (Semver, error) {
	var best Semver
	found := false
	for page := 1; page <= maxTagPages; page++ {
		names, err := fetchTagPage(ctx, client, page)
		if err != nil {
			return Semver{}, err
		}
		for _, name := range names {
			v, ok := Parse(name)
			if !ok || !v.Stable() {
				continue
			}
			if !found || Compare(v, best) > 0 {
				best, found = v, true
			}
		}
		if len(names) < 100 {
			break
		}
	}
	if !found {
		return Semver{}, fmt.Errorf("no stable release tag found")
	}
	return best, nil
}

func fetchTagPage(ctx context.Context, client *http.Client, page int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL+"?per_page=100&page="+strconv.Itoa(page), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "docktail/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API answered %s", resp.Status)
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decode tags: %w", err)
	}
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	return names, nil
}
