// Package protocol defines the versioned wire protocol between frames
// and the management server.
//
// The protocol is versioned independently of the application version so
// that a frame can reject (or defer) server behaviour it does not
// understand, and vice versa. Current release ships API v1.
package protocol

// APIPath is the versioned REST API prefix served by the management
// server (M2+). M1 exposes /healthz at the root for load balancers and
// the admin UI.
const APIPath = "/api/v1"

// ManifestProtocol identifies the manifest synchronisation protocol
// (revisions + ETags, conditional requests, content hashes) that the
// frame sync engine (M3) and server manifest endpoint (M3) share.
const ManifestProtocol = "openframe-manifest/1"

// HealthPath is the server liveness endpoint used by frames, the admin
// UI and `openframe doctor`.
const HealthPath = "/healthz"

// MinProtocolVersion is the oldest manifest protocol this build
// understands. Release metadata (M6) carries the same kind of
// compatibility bound for the binary itself.
const MinProtocolVersion = 1
const MaxProtocolVersion = 1
