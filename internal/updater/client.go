package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// Release-server URL scheme (served by the management server, M6,
// spec §14). The metadata and artifact endpoints are public: releases
// are self-authenticating (signed), and an unavailable server must
// never be a precondition of slideshow or sync. The platform is a
// query parameter: one server hosts many (os, arch) release lines.
func LatestURL(server, channel, goos, goarch string) string {
	return strings.TrimRight(server, "/") + "/api/v1/releases/" + url.PathEscape(channel) +
		"/latest.json?os=" + url.QueryEscape(goos) + "&arch=" + url.QueryEscape(goarch)
}

func ArtifactURL(server, version, goos, goarch string) string {
	return strings.TrimRight(server, "/") + "/api/v1/releases/" + url.PathEscape(version) +
		"/artifact?os=" + url.QueryEscape(goos) + "&arch=" + url.QueryEscape(goarch)
}

// HTTPReleaseSource fetches releases from a release server over
// HTTP(S). It is the production counterpart of the injected
// FetchRelease/DownloadArtifact hooks in tests.
type HTTPReleaseSource struct {
	ServerURL string
	HTTP      *http.Client
	// DeviceToken, when set, is sent as a bearer token (frames that
	// are paired can present it; the endpoints accept it but do not
	// require it).
	DeviceToken string
	// TargetOS/TargetArch select the release line (empty: this
	// process's platform).
	TargetOS   string
	TargetArch string
}

// NewHTTPReleaseSource builds a source with sane defaults.
func NewHTTPReleaseSource(serverURL string) *HTTPReleaseSource {
	return &HTTPReleaseSource{
		ServerURL:  serverURL,
		HTTP:       &http.Client{Timeout: 5 * time.Minute},
		TargetOS:   runtime.GOOS,
		TargetArch: runtime.GOARCH,
	}
}

func (s *HTTPReleaseSource) platform() (string, string) {
	osName, arch := s.TargetOS, s.TargetArch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return osName, arch
}

// FetchRelease retrieves the latest signed release for a channel.
// A 404 (no release published) is an error of kind ErrNoRelease.
var ErrNoRelease = fmt.Errorf("updater: no release published for channel")

// latestEnvelope mirrors the server's latest.json body: the signed
// release verbatim plus an unsigned serving block (operator rollout
// override — policy only, see docs/updater/trust-model.md).
type latestEnvelope struct {
	Release json.RawMessage `json:"release"`
	Serving struct {
		RolloutPercent int      `json:"rollout_percent"`
		RolloutDevices []string `json:"rollout_devices,omitempty"`
	} `json:"serving"`
}

// ServingOverride is the server's unsigned rollout policy for the
// latest release (operator-controlled staged rollout). It may change
// WHO is eligible for an update — never WHAT: version, artifact
// digest and size live in the signed document, and the signature must
// verify against the document untouched (see trust-model.md).
type ServingOverride struct {
	RolloutPercent int
	RolloutDevices []string
}

// FetchedRelease is a signed release plus the optional serving
// override that accompanies it on the wire.
type FetchedRelease struct {
	Release Release
	Serving *ServingOverride // nil: use the signed rollout fields
}

// FetchReleaseEx retrieves the latest release WITH its serving
// override. The Release is always returned verbatim as signed — the
// coordinator applies the override only to the rollout-eligibility
// decision.
func (s *HTTPReleaseSource) FetchReleaseEx(ctx context.Context, channel string) (FetchedRelease, error) {
	osName, arch := s.platform()
	body, err := s.getRaw(ctx, LatestURL(s.ServerURL, channel, osName, arch))
	if err != nil {
		return FetchedRelease{}, err
	}
	var env latestEnvelope
	if err := json.Unmarshal(body, &env); err == nil && len(env.Release) > 0 {
		var rel Release
		if err := json.Unmarshal(env.Release, &rel); err != nil {
			return FetchedRelease{}, fmt.Errorf("updater: envelope release: %w", err)
		}
		return FetchedRelease{
			Release: rel,
			Serving: &ServingOverride{
				RolloutPercent: env.Serving.RolloutPercent,
				RolloutDevices: env.Serving.RolloutDevices,
			},
		}, nil
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return FetchedRelease{}, fmt.Errorf("updater: decode latest: %w", err)
	}
	return FetchedRelease{Release: rel}, nil
}

// FetchRelease implements Coordinator.FetchRelease (no override
// information; the signed rollout fields apply).
func (s *HTTPReleaseSource) FetchRelease(ctx context.Context, channel string) (Release, error) {
	fr, err := s.FetchReleaseEx(ctx, channel)
	if err != nil {
		return Release{}, err
	}
	return fr.Release, nil
}

// DownloadArtifact implements Coordinator.DownloadArtifact.
func (s *HTTPReleaseSource) DownloadArtifact(ctx context.Context, rel Release) (io.ReadCloser, error) {
	osName, arch := s.platform()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ArtifactURL(s.ServerURL, rel.Version, osName, arch), nil)
	if err != nil {
		return nil, err
	}
	if s.DeviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.DeviceToken)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("updater: artifact %s: %s: %s", rel.Version, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

func (s *HTTPReleaseSource) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

// getRaw fetches u and returns the body (404 → ErrNoRelease).
func (s *HTTPReleaseSource) getRaw(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if s.DeviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.DeviceToken)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("updater: %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
