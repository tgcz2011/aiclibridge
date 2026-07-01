// Package update implements GitHub-release-based "is there a newer
// aiclibridge?" detection. It is used by the `aiclibridge update check`
// subcommand and (optionally) by an inline notice printed at daemon
// startup.
//
// The package is deliberately self-contained: it depends only on the
// standard library + the project's own version constant, so it can be
// reused by the CLI, the daemon, and tests without dragging in the
// facade or API layer. The HTTP call is bounded by a caller-supplied
// context so a hung GitHub API never blocks daemon startup.
//
// # Version comparison
//
// Versions are dot-separated numeric runs ("0.4.1", "v1.2.3"), compared
// segment by segment. A missing segment is treated as 0 (so "1.2" ==
// "1.2.0"). Non-numeric segments (e.g. "-rc1", "beta") are not supported
// and fall back to a lexical comparison so a release like "1.0.0-rc1"
// is still ordered deterministically rather than rejected.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultOwner and DefaultRepo pin the GitHub source. They are exported
// so callers (e.g. the install scripts' docs) can reference them, and so
// tests can override them via CheckWithRepo.
const (
	DefaultOwner = "tgcz2011"
	DefaultRepo  = "aiclibridge"

	// apiLatestURL is the GitHub REST endpoint for the latest release.
	// GitHub returns 404 when no non-prerelease, non-draft release
	// exists; CheckLatest surfaces that as ErrNoReleases.
	apiLatestURL = "https://api.github.com/repos/%s/%s/releases/latest"
)

// ErrNoReleases is returned by CheckLatest when GitHub reports no
// publishable release (404). Callers usually treat this as "no update
// info available" rather than a hard failure.
var ErrNoReleases = errors.New("update: no GitHub releases found")

// HTTPClient is the minimal client surface CheckLatest needs. The
// production caller passes an *http.Client; tests pass a stub. A
// reasonable default timeout (10s) is applied when the caller passes
// nil — short enough that a hung GitHub API does not block daemon
// startup, long enough for a slow mobile connection.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Release describes the subset of GitHub's release JSON that this
// package consumes. The field set is intentionally small so future
// GitHub API additions (drafts, discussions, etc.) do not break the
// parser.
type Release struct {
	// TagName is the git tag, e.g. "v0.4.1". This is the canonical
	// version identifier for a release.
	TagName string `json:"tag_name"`
	// Name is the human-readable release title (often == TagName).
	Name string `json:"name"`
	// HTMLURL is the web UI link to the release page.
	HTMLURL string `json:"html_url"`
	// Body is the release notes markdown (the "body" field on GitHub).
	Body string `json:"body"`
	// PublishedAt is the ISO-8601 timestamp of the release.
	PublishedAt string `json:"published_at"`
	// Assets is the list of downloadable files attached to the release.
	Assets []Asset `json:"assets"`
}

// Asset is one downloadable file on a GitHub release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Info is the high-level answer to "should the user upgrade?". It is
// returned by Check, the convenience wrapper that combines CheckLatest
// + CompareVersions. The CLI formats this into a one-paragraph notice.
type Info struct {
	// Current is the version the running binary was built with (the
	// string passed to Check), without a leading 'v'.
	Current string
	// Latest is the newest release tag, without a leading 'v'.
	Latest string
	// LatestTag is the original tag form ("v0.4.1") — useful for the
	// install / upgrade instructions.
	LatestTag string
	// HasUpdate is true when Latest is strictly greater than Current.
	HasUpdate bool
	// HTMLURL is the release page link.
	HTMLURL string
	// AssetURL is the per-platform asset URL for the current GOOS/GOARCH,
	// or empty if no matching asset was found on the release.
	AssetURL string
	// ReleaseNotes is the markdown body of the release (truncated to a
	// reasonable length by the caller).
	ReleaseNotes string
}

// CheckLatest fetches the latest non-prerelease, non-draft release from
// GitHub. Returns ErrNoReleases when GitHub responds 404 (no
// publishable release yet). The caller controls the timeout via ctx.
//
// client may be nil — a 10s-timeout *http.Client is used as a default.
// A non-2xx response (other than 404) is surfaced as an error wrapping
// the status so the caller can distinguish "no releases" from "GitHub
// is broken".
func CheckLatest(ctx context.Context, client HTTPClient, owner, repo string) (*Release, error) {
	if owner == "" {
		owner = DefaultOwner
	}
	if repo == "" {
		repo = DefaultRepo
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	url := fmt.Sprintf(apiLatestURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("update: build request: %w", err)
	}
	// GitHub asks for an Accept header for the REST API; the v3 JSON
	// representation is the default and most stable.
	req.Header.Set("Accept", "application/vnd.github+json")
	// User-Agent is recommended by GitHub to identify the caller; some
	// rate-limit tiers reject empty agents.
	req.Header.Set("User-Agent", "aiclibridge-update-check")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoReleases
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("update: GitHub API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("update: decode release JSON: %w", err)
	}
	return &rel, nil
}

// Check is the convenience wrapper used by the CLI: it fetches the
// latest release, compares currentVersion to the latest tag, and picks
// the matching asset URL for (goos, goarch). The asset selection
// mirrors the install scripts' naming convention
// `aiclibridge-{goos}-{goarch}.{tar.gz|zip}`.
//
// All errors are returned wrapped; the CLI prints them as a warning and
// continues, since update-check is best-effort.
func Check(ctx context.Context, client HTTPClient, owner, repo, currentVersion, goos, goarch string) (*Info, error) {
	rel, err := CheckLatest(ctx, client, owner, repo)
	if err != nil {
		return nil, err
	}

	latest := stripV(rel.TagName)
	current := stripV(currentVersion)

	info := &Info{
		Current:      current,
		Latest:       latest,
		LatestTag:    rel.TagName,
		HTMLURL:      rel.HTMLURL,
		ReleaseNotes: rel.Body,
		HasUpdate:    CompareVersions(current, latest) < 0,
	}

	// Pick the asset matching the running GOOS/GOARCH. The release
	// asset naming is `aiclibridge-{goos}-{goarch}.tar.gz` (Unix) or
	// `aiclibridge-{goos}-{goarch}.zip` (Windows). We match on the
	// prefix so the extension is irrelevant.
	prefix := fmt.Sprintf("aiclibridge-%s-%s.", goos, goarch)
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, prefix) {
			info.AssetURL = a.BrowserDownloadURL
			break
		}
	}

	return info, nil
}

// CompareVersions compares two version strings segment by segment.
// Returns -1 if a < b, 0 if a == b, +1 if a > b. A leading 'v' is
// stripped from each side ("v0.4.1" == "0.4.1"). Missing trailing
// segments are treated as 0 ("1.2" == "1.2.0").
//
// Non-numeric segments (e.g. "rc1", "beta") fall back to lexical
// comparison so pre-release versions are still ordered deterministically
// rather than rejected as unparseable.
func CompareVersions(a, b string) int {
	aa := splitVersion(stripV(a))
	bb := splitVersion(stripV(b))
	n := len(aa)
	if len(bb) > n {
		n = len(bb)
	}
	for i := 0; i < n; i++ {
		av, bokA := partInt(aa, i)
		bv, bokB := partInt(bb, i)
		if bokA && bokB {
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
			continue
		}
		// At least one side is non-numeric: fall back to lexical
		// comparison of the raw segments so ordering stays stable.
		as := partStr(aa, i)
		bs := partStr(bb, i)
		if as < bs {
			return -1
		}
		if as > bs {
			return 1
		}
	}
	return 0
}

// stripV removes a single leading 'v' or 'V' from a version string.
// "v0.4.1" → "0.4.1"; "0.4.1" → "0.4.1"; "V1.2" → "1.2".
func stripV(s string) string {
	if len(s) >= 2 && (s[0] == 'v' || s[0] == 'V') {
		// Only strip if the next char is a digit — "version" stays.
		if s[1] >= '0' && s[1] <= '9' {
			return s[1:]
		}
	}
	return s
}

// splitVersion splits "0.4.1" into ["0","4","1"]. A leading 'v' is
// stripped first. Empty input returns ["0"] so CompareVersions treats
// "" == "0.0.0".
func splitVersion(s string) []string {
	s = stripV(s)
	if s == "" {
		return []string{"0"}
	}
	// Split on '.' but also handle '-' (e.g. "1.0.0-rc1") by treating
	// the suffix as its own segment so pre-release versions compare
	// against their numeric prefix deterministically.
	s = strings.ReplaceAll(s, "-", ".")
	parts := strings.Split(s, ".")
	if len(parts) == 0 {
		return []string{"0"}
	}
	return parts
}

// partInt returns the integer value of parts[i]. An out-of-range index
// is treated as 0 with ok=true so "1.2" compares equal to "1.2.0"
// (missing trailing segments are zero). A non-numeric in-range segment
// returns 0 + ok=false so the caller falls back to lexical comparison
// for pre-release suffixes like "rc1" or "beta".
func partInt(parts []string, i int) (int, bool) {
	if i < 0 || i >= len(parts) {
		return 0, true
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// partStr returns parts[i] (or "" when out of range) for the lexical
// fallback path.
func partStr(parts []string, i int) string {
	if i < 0 || i >= len(parts) {
		return ""
	}
	return parts[i]
}

// FormatNotice renders an Info as a multi-line human-readable string.
// The CLI prints this to stderr so it does not pollute stdout piping.
// When HasUpdate is false, the notice is empty (the caller may still
// print a one-line "you're up to date" message).
func FormatNotice(info *Info) string {
	if info == nil {
		return ""
	}
	if !info.HasUpdate {
		return fmt.Sprintf("aiclibridge %s is up to date.\n", info.Current)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "aiclibridge update available: %s → %s\n", info.Current, info.LatestTag)
	fmt.Fprintf(&sb, "release: %s\n", info.HTMLURL)
	if info.AssetURL != "" {
		fmt.Fprintf(&sb, "asset:  %s\n", info.AssetURL)
	}
	// One-line upgrade hint pointing at the install script. We pick
	// the Unix form by default; the caller can override on Windows.
	fmt.Fprintf(&sb, "upgrade: curl -fsSL https://github.com/%s/%s/raw/main/scripts/install.sh | sh\n",
		DefaultOwner, DefaultRepo)
	return sb.String()
}
