package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── CompareVersions ──

// TestCompareVersions covers every documented behaviour: numeric
// comparison, v-prefix stripping, missing trailing segments, equal
// versions, and the lexical fallback for non-numeric segments.
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal", "0.4.1", "0.4.1", 0},
		{"equal_with_v", "v0.4.1", "0.4.1", 0},
		{"equal_V_prefix", "V1.2", "1.2", 0},
		{"a_smaller", "0.4.0", "0.4.1", -1},
		{"a_larger", "0.5.0", "0.4.9", 1},
		{"missing_trailing_zero", "1.2", "1.2.0", 0},
		{"major_jump", "0.9.9", "1.0.0", -1},
		{"empty_equal_zero", "", "0.0.0", 0},
		{"empty_smaller", "", "0.0.1", -1},
		// Pre-release suffixes are compared lexically against the
		// counterpart segment. "rc1" is non-empty so it sorts AFTER
		// an out-of-range (empty) segment — this is NOT semver, just
		// deterministic ordering so prereleases don't crash the
		// comparator. Callers wanting semver semantics should compare
		// the numeric prefix themselves.
		{"non_numeric_lexical_rc", "1.0.0-rc1", "1.0.0", 1},          // "rc1" > ""
		{"non_numeric_lexical_beta", "1.0.0-beta", "1.0.0-rc1", -1}, // "beta" < "rc1"
		{"two_digit_segments", "10.0.0", "9.99.99", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompareVersions(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestStripV verifies that stripV only strips a leading 'v'/'V' when it
// is followed by a digit, so "version" and "vigor" are preserved.
func TestStripV(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"v0.4.1", "0.4.1"},
		{"V1.2", "1.2"},
		{"0.4.1", "0.4.1"},
		{"version", "version"},   // not followed by digit — kept
		{"vigor", "vigor"},       // not followed by digit — kept
		{"v", "v"},               // too short to strip
		{"", ""},
		{"vABC", "vABC"},         // 'A' is not a digit
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := stripV(tt.in); got != tt.want {
				t.Errorf("stripV(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSplitVersion verifies the segment splitter handles the empty
// input case and the '-' → '.' normalisation for pre-release suffixes.
func TestSplitVersion(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"0.4.1", []string{"0", "4", "1"}},
		{"v1.2", []string{"1", "2"}},
		{"", []string{"0"}},
		{"1.0.0-rc1", []string{"1", "0", "0", "rc1"}},
		{"1.0.0-beta.2", []string{"1", "0", "0", "beta", "2"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := splitVersion(tt.in)
			if !equalSlices(got, tt.want) {
				t.Errorf("splitVersion(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── CheckLatest (live HTTP via httptest) ──

// TestCheckLatestOK stubs GitHub's releases/latest endpoint with a
// realistic payload and verifies the parsed Release.
func TestCheckLatestOK(t *testing.T) {
	payload := `{
		"tag_name": "v0.4.1",
		"name": "v0.4.1",
		"html_url": "https://github.com/tgcz2011/aiclibridge/releases/tag/v0.4.1",
		"body": "## v0.4.1\n\nWindows binary ships.",
		"published_at": "2026-07-01T12:30:00Z",
		"assets": [
			{"name": "aiclibridge-darwin-arm64.tar.gz", "browser_download_url": "https://github.com/tgcz2011/aiclibridge/releases/download/v0.4.1/aiclibridge-darwin-arm64.tar.gz", "size": 4862968},
			{"name": "aiclibridge-windows-amd64.zip", "browser_download_url": "https://github.com/tgcz2011/aiclibridge/releases/download/v0.4.1/aiclibridge-windows-amd64.zip", "size": 5100000}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request shape: Accept + User-Agent headers.
		if got := r.Header.Get("Accept"); !strings.Contains(got, "vnd.github") {
			t.Errorf("Accept header: got %q, want vnd.github", got)
		}
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Errorf("User-Agent header is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	// Override the URL template by calling CheckLatest with an explicit
	// owner/repo and pointing the package at the test server. Since
	// apiLatestURL is a constant, we can't override it directly — so we
	// build the request via the exported function and accept the real
	// URL form. Instead, verify by hitting the test server via a custom
	// HTTPClient that rewrites the URL.
	client := urlRewriteClient{newBase: srv.URL}
	rel, err := CheckLatest(context.Background(), client, DefaultOwner, DefaultRepo)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if rel.TagName != "v0.4.1" {
		t.Errorf("TagName: got %q, want v0.4.1", rel.TagName)
	}
	if len(rel.Assets) != 2 {
		t.Errorf("Assets: got %d, want 2", len(rel.Assets))
	}
	if rel.Assets[0].Name != "aiclibridge-darwin-arm64.tar.gz" {
		t.Errorf("Asset[0].Name: got %q", rel.Assets[0].Name)
	}
}

// TestCheckLatestNotFound verifies that a 404 surfaces as ErrNoReleases.
func TestCheckLatestNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	client := urlRewriteClient{newBase: srv.URL}
	_, err := CheckLatest(context.Background(), client, DefaultOwner, DefaultRepo)
	if !errors.Is(err, ErrNoReleases) {
		t.Errorf("CheckLatest 404: got err %v, want ErrNoReleases", err)
	}
}

// TestCheckLatest5xx verifies that a non-404 error surfaces as a wrapped
// error containing the status code, so the caller can distinguish
// "no releases" from "GitHub is broken".
func TestCheckLatest5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"server error"}`)
	}))
	defer srv.Close()

	client := urlRewriteClient{newBase: srv.URL}
	_, err := CheckLatest(context.Background(), client, DefaultOwner, DefaultRepo)
	if err == nil {
		t.Fatalf("CheckLatest 503: expected error, got nil")
	}
	if errors.Is(err, ErrNoReleases) {
		t.Errorf("CheckLatest 503: err is ErrNoReleases, want a 503-wrapped error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("CheckLatest 503: err %q does not contain '503'", err.Error())
	}
}

// TestCheckLatestBadJSON verifies that a 200 with garbage JSON returns a
// decode error rather than panicking or returning a partial Release.
func TestCheckLatestBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `not valid json`)
	}))
	defer srv.Close()

	client := urlRewriteClient{newBase: srv.URL}
	_, err := CheckLatest(context.Background(), client, DefaultOwner, DefaultRepo)
	if err == nil {
		t.Fatalf("CheckLatest bad JSON: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("CheckLatest bad JSON: err %q does not mention decode", err.Error())
	}
}

// ── Check (high-level wrapper) ──

// TestCheckHasUpdate verifies the high-level Check picks the right asset
// and sets HasUpdate correctly when current < latest.
func TestCheckHasUpdate(t *testing.T) {
	payload := `{
		"tag_name": "v0.5.0",
		"name": "v0.5.0",
		"html_url": "https://github.com/tgcz2011/aiclibridge/releases/tag/v0.5.0",
		"body": "## v0.5.0\n\ninstall scripts + auto-update",
		"assets": [
			{"name": "aiclibridge-darwin-arm64.tar.gz", "browser_download_url": "https://example.com/darwin-arm64.tar.gz", "size": 1},
			{"name": "aiclibridge-windows-amd64.zip", "browser_download_url": "https://example.com/windows-amd64.zip", "size": 1}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	client := urlRewriteClient{newBase: srv.URL}
	info, err := Check(context.Background(), client, DefaultOwner, DefaultRepo, "0.4.1", "darwin", "arm64")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.HasUpdate {
		t.Errorf("HasUpdate: got false, want true (current=0.4.1, latest=0.5.0)")
	}
	if info.Current != "0.4.1" {
		t.Errorf("Current: got %q, want 0.4.1", info.Current)
	}
	if info.Latest != "0.5.0" {
		t.Errorf("Latest: got %q, want 0.5.0", info.Latest)
	}
	if info.LatestTag != "v0.5.0" {
		t.Errorf("LatestTag: got %q, want v0.5.0", info.LatestTag)
	}
	if info.AssetURL != "https://example.com/darwin-arm64.tar.gz" {
		t.Errorf("AssetURL: got %q, want darwin-arm64 url", info.AssetURL)
	}
}

// TestCheckNoUpdate verifies HasUpdate is false when current == latest.
func TestCheckNoUpdate(t *testing.T) {
	payload := `{"tag_name":"v0.4.1","name":"v0.4.1","html_url":"https://example.com","body":"","assets":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	client := urlRewriteClient{newBase: srv.URL}
	info, err := Check(context.Background(), client, DefaultOwner, DefaultRepo, "0.4.1", "linux", "amd64")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.HasUpdate {
		t.Errorf("HasUpdate: got true, want false (current==latest)")
	}
	if info.AssetURL != "" {
		t.Errorf("AssetURL: got %q, want empty (no assets on release)", info.AssetURL)
	}
}

// TestCheckWindowsAsset verifies the asset picker matches the .zip
// (Windows) form, not just .tar.gz.
func TestCheckWindowsAsset(t *testing.T) {
	payload := `{
		"tag_name":"v0.5.0","name":"v0.5.0","html_url":"https://example.com","body":"",
		"assets":[{"name":"aiclibridge-windows-amd64.zip","browser_download_url":"https://example.com/win.zip","size":1}]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	client := urlRewriteClient{newBase: srv.URL}
	info, err := Check(context.Background(), client, DefaultOwner, DefaultRepo, "0.4.1", "windows", "amd64")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.AssetURL != "https://example.com/win.zip" {
		t.Errorf("AssetURL: got %q, want win.zip", info.AssetURL)
	}
}

// ── FormatNotice ──

// TestFormatNoticeUpToDate verifies the "you're up to date" line.
func TestFormatNoticeUpToDate(t *testing.T) {
	info := &Info{Current: "0.4.1", HasUpdate: false}
	got := FormatNotice(info)
	if want := "aiclibridge 0.4.1 is up to date.\n"; got != want {
		t.Errorf("FormatNotice up-to-date: got %q, want %q", got, want)
	}
}

// TestFormatNoticeHasUpdate verifies the multi-line notice includes the
// current → latest arrow, the release URL, the asset URL, and an
// upgrade hint.
func TestFormatNoticeHasUpdate(t *testing.T) {
	info := &Info{
		Current:   "0.4.1",
		LatestTag: "v0.5.0",
		HTMLURL:   "https://github.com/tgcz2011/aiclibridge/releases/tag/v0.5.0",
		AssetURL:  "https://example.com/darwin-arm64.tar.gz",
		HasUpdate: true,
	}
	got := FormatNotice(info)
	for _, want := range []string{
		"0.4.1 → v0.5.0",
		"release: https://github.com/tgcz2011/aiclibridge/releases/tag/v0.5.0",
		"asset:  https://example.com/darwin-arm64.tar.gz",
		"upgrade: curl -fsSL https://github.com/tgcz2011/aiclibridge/raw/main/scripts/install.sh | sh",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatNotice: output missing %q\nfull output:\n%s", want, got)
		}
	}
}

// TestFormatNoticeNil verifies a nil Info renders as an empty string.
func TestFormatNoticeNil(t *testing.T) {
	if got := FormatNotice(nil); got != "" {
		t.Errorf("FormatNotice(nil): got %q, want empty", got)
	}
}

// ── helpers ──

// urlRewriteClient is a stub HTTPClient that rewrites every request's
// host to newBase, so tests can point CheckLatest at a httptest.Server
// without modifying the package's URL constant.
type urlRewriteClient struct {
	newBase string
}

func (c urlRewriteClient) Do(req *http.Request) (*http.Response, error) {
	// Replace the request URL's scheme + host with the test server's,
	// preserving the path so the handler still sees /repos/.../latest.
	req.URL.Scheme = strings.SplitN(c.newBase, "://", 2)[0]
	req.URL.Host = strings.SplitN(strings.SplitN(c.newBase, "://", 2)[1], "/", 2)[0]
	// Use a fresh client so we don't recurse.
	return http.DefaultClient.Do(req)
}

// Ensure the JSON decoder path doesn't choke on the Release struct by
// round-tripping a payload through json.Marshal/Unmarshal once.
func TestReleaseJSONRoundTrip(t *testing.T) {
	in := Release{
		TagName:     "v0.4.1",
		Name:        "v0.4.1",
		HTMLURL:     "https://example.com",
		Body:        "## v0.4.1\n\nwindows binary",
		PublishedAt: "2026-07-01T12:30:00Z",
		Assets: []Asset{
			{Name: "aiclibridge-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.com/darwin-arm64.tar.gz", Size: 4862968},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Release
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TagName != in.TagName {
		t.Errorf("TagName round-trip: got %q, want %q", out.TagName, in.TagName)
	}
	if len(out.Assets) != 1 || out.Assets[0].Size != in.Assets[0].Size {
		t.Errorf("Assets round-trip mismatch: got %#v", out.Assets)
	}
}
