// Package pairing is the frame-side client for the device pairing
// protocol (spec §12). An unpaired frame:
//
//  1. requests a challenge (its own identity + a display name),
//  2. displays the returned code on its screen, and
//  3. polls the challenge status until an administrator approves it.
//
// On approval the server registers the frame under its own identity
// and issues a credential; the frame stores it (see
// frame.SaveCredential) and synchronisation begins. All traffic is
// outbound HTTPS from the frame — the server never connects in
// (spec §12).
package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Challenge is the frame-visible result of a challenge request.
type Challenge struct {
	ID         string `json:"challenge_id"`
	Code       string `json:"code"`
	ExpiresAt  string `json:"expires_at"`
	TTLSeconds int    `json:"ttl_seconds"`
}

// Status is the outcome of a status poll. Credential is present
// exactly once: on the first poll after approval (one-time claim).
type Status struct {
	State      string `json:"status"`     // pending | approved | rejected | expired
	DeviceID   string `json:"device_id"`  // present when approved
	Credential string `json:"credential"` // one-time claim, when approved
}

// State constants (mirror the server's).
const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateRejected = "rejected"
	StateExpired  = "expired"
)

var ErrServerDown = errors.New("pairing: server unreachable")

// AlreadyRegistered is returned when the server knows this frame
// (409). ChallengeID, when non-empty, is a resolvable challenge the
// frame should resume polling — typically an approved challenge whose
// one-time credential was never claimed before the frame restarted.
type AlreadyRegistered struct {
	ChallengeID string
}

func (e *AlreadyRegistered) Error() string {
	return "pairing: device already registered"
}

type client struct {
	base  string
	httpc *http.Client
}

// newClient builds a pairing client for serverURL, preserving the
// operator's scheme (OpenFrame is LAN/self-hosted; TLS is a
// deployment concern, not a protocol one — the server may sit behind
// a reverse proxy, spec §19).
func newClient(serverURL string) (*client, error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("pairing: invalid server url %q", serverURL)
	}
	return &client{
		base:  u.Scheme + "://" + u.Host,
		httpc: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// RequestChallenge asks the server for a pairing challenge for this
// frame identity.
func RequestChallenge(ctx context.Context, serverURL, deviceID, deviceName string) (*Challenge, error) {
	c, err := newClient(serverURL)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{
		"device_id":   deviceID,
		"device_name": deviceName,
	})
	var out Challenge
	if err := c.do(ctx, http.MethodPost, "/api/v1/devices/pair-challenges", body, &out); err != nil {
		return nil, err
	}
	if out.Code == "" {
		return nil, errors.New("pairing: server returned an empty code")
	}
	return &out, nil
}

// PollStatus checks the outcome of a challenge.
func PollStatus(ctx context.Context, serverURL, challengeID string) (*Status, error) {
	c, err := newClient(serverURL)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"challenge_id": challengeID})
	var out Status
	if err := c.do(ctx, http.MethodPost, "/api/v1/devices/pair-status", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServerDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if resp.StatusCode == http.StatusConflict {
			var body struct {
				Error       string `json:"error"`
				ChallengeID string `json:"challenge_id"`
			}
			if json.Unmarshal(b, &body) == nil && body.Error == "device_registered" {
				return &AlreadyRegistered{ChallengeID: body.ChallengeID}
			}
		}
		return fmt.Errorf("pairing: %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
