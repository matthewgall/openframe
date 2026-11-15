package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/updater"
	"github.com/matthewgall/openframe/migrations"
)

// cmd_release: operator tooling for the secure release pipeline (M6 /
// ADR-0007). It exists because the release flow — Ed25519 keypair,
// canonical metadata, signature, multipart publish — should not be
// something an operator assembles by hand.
//
// Subcommands:
//
//	keygen   create a release-signing keypair (private key + the
//	         device-pinned trusted-keys.json)
//	sign     hash an artifact, build the metadata, sign it, self-verify
//	verify   check a signed metadata file against a trusted-keys file
//	publish  upload metadata + artifact to the server (admin login)
//
// Trust model reminder: the server stores metadata verbatim and
// trusts no signature; the FRAME's pinned trusted-keys.json is
// authoritative. The files keygen produces go to different places:
// the private key stays on the release machine, the trust file is
// deployed to every frame device.
func runRelease(args []string) int {
	if len(args) == 0 {
		releaseUsage()
		return ExitUsage
	}
	switch args[0] {
	case "keygen":
		return releaseKeygen(args[1:])
	case "sign":
		return releaseSign(args[1:])
	case "verify":
		return releaseVerify(args[1:])
	case "publish":
		return releasePublish(args[1:])
	case "help", "-h", "--help":
		releaseUsage()
		return ExitOK
	}
	fmt.Fprintf(os.Stderr, "unknown release subcommand %q\n\n", args[0])
	releaseUsage()
	return ExitUsage
}

func releaseUsage() {
	fmt.Fprint(os.Stderr, `openframe release — create, sign and publish releases

Usage:
  openframe release keygen   --key-id ID [--out-dir DIR] [--note TEXT]
  openframe release sign     --version V --channel C --os OS --arch ARCH
                             --artifact FILE --key FILE
                             [--min-protocol N] [--min-schema N]
                             [--rollout 0-100] [--canary ID[,ID...]]
                             [--not-before RFC3339] [--expires-at RFC3339]
                             [--rollback] [--out FILE]
  openframe release verify   --metadata FILE --trust FILE
  openframe release publish  --server URL --email E --password P
                             --metadata FILE --artifact FILE

The private key never leaves the release machine. The trust file
(keygen output) is deployed to each frame at <root>/trust/trusted-keys.json
and is the only thing frames accept releases against.
`)
}

// --- keygen -------------------------------------------------------------------

type releasePrivateKey struct {
	KeyID string `json:"key_id"`
	// Seed is the Ed25519 seed, base64 (32 raw bytes). The public key
	// is derived from it; storing only the seed keeps one file
	// authoritative.
	Seed string `json:"seed"`
	Note string `json:"note,omitempty"`
}

func (k *releasePrivateKey) PrivateKey() (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(k.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("release: bad key seed (want base64 %d bytes)", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func releaseKeygen(args []string) int {
	fs := flag.NewFlagSet("release keygen", flag.ExitOnError)
	keyID := fs.String("key-id", "", "key id (e.g. openframe-2026)")
	outDir := fs.String("out-dir", ".", "directory for the output files")
	note := fs.String("note", "", "free-form label stored in the trust file")
	_ = fs.Parse(args)
	if *keyID == "" || strings.ContainsAny(*keyID, ` /\`) {
		fmt.Fprintln(os.Stderr, "keygen: --key-id is required (letters, digits, - and _ only)")
		return ExitUsage
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		return ExitError
	}
	priv := ed25519.NewKeyFromSeed(seed)
	if *note == "" {
		*note = "release signing key, generated " + time.Now().UTC().Format(time.RFC3339)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		return ExitError
	}

	privPath := filepath.Join(*outDir, "release-key-"+*keyID+".private")
	privFile := releasePrivateKey{
		KeyID: *keyID,
		Seed:  base64.StdEncoding.EncodeToString(seed),
		Note:  *note,
	}
	b, _ := json.MarshalIndent(privFile, "", "  ")
	if err := os.WriteFile(privPath, append(b, '\n'), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		return ExitError
	}

	trustPath := filepath.Join(*outDir, "trusted-keys-"+*keyID+".json")
	trustFile := updater.TrustedKeys{Keys: map[string]updater.TrustedKey{
		*keyID: {
			KeyID:   *keyID,
			Ed25519: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
			Note:    *note,
		},
	}}
	tb, _ := json.MarshalIndent(trustFile, "", "  ")
	if err := os.WriteFile(trustPath, append(tb, '\n'), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "keygen:", err)
		return ExitError
	}

	fmt.Printf("release key %q generated\n\n", *keyID)
	fmt.Printf("  %s\n    PRIVATE KEY — sign releases with this; keep it on the release\n    machine only. Losing it means every future release needs a\n    key rotation signed by a key frames already trust.\n", privPath)
	fmt.Printf("  %s\n    DEVICE TRUST FILE — deploy to every frame as\n    <root>/trust/trusted-keys.json (see docs/updater/deployment.md).\n", trustPath)
	fmt.Printf("\nNext: openframe release sign --version <V> --channel <C> \\\n  --os <os> --arch <arch> --artifact <binary> --key %s\n", privPath)
	return ExitOK
}

// --- sign ---------------------------------------------------------------------

func releaseSign(args []string) int {
	fs := flag.NewFlagSet("release sign", flag.ExitOnError)
	version := fs.String("version", "", "version, MAJOR.MINOR.PATCH")
	channel := fs.String("channel", "", "stable | beta | development")
	osName := fs.String("os", "", "target os (linux, darwin, windows)")
	arch := fs.String("arch", "", "target arch (amd64, arm64, arm, 386, riscv64)")
	artifact := fs.String("artifact", "", "path to the built binary")
	keyFile := fs.String("key", "", "release private key file (keygen output)")
	minProto := fs.Int("min-protocol", 1, "oldest manifest protocol this binary speaks")
	minSchema := fs.Int("min-schema", 0, "oldest frame schema this binary can start with (0 = current)")
	rollout := fs.Int("rollout", 100, "rollout percentage 0-100")
	canary := fs.String("canary", "", "comma-separated device ids always in the rollout")
	notBefore := fs.String("not-before", "", "RFC3339 lower time bound")
	expiresAt := fs.String("expires-at", "", "RFC3339 hard expiry")
	isRollback := fs.Bool("rollback", false, "mark as an intentional rollback target")
	out := fs.String("out", "", "metadata output file (default: derived name)")
	_ = fs.Parse(args)

	for _, req := range []struct {
		name string
		v    *string
	}{{"--version", version}, {"--channel", channel}, {"--os", osName}, {"--arch", arch},
		{"--artifact", artifact}, {"--key", keyFile}} {
		if *req.v == "" {
			fmt.Fprintf(os.Stderr, "sign: %s is required\n", req.name)
			return ExitUsage
		}
	}
	if !updater.ValidChannel(*channel) {
		fmt.Fprintln(os.Stderr, "sign: --channel must be stable, beta or development")
		return ExitUsage
	}
	if _, err := updater.ParseVersion(*version); err != nil {
		fmt.Fprintf(os.Stderr, "sign: bad --version: %v\n", err)
		return ExitUsage
	}
	if *rollout < 0 || *rollout > 100 {
		fmt.Fprintln(os.Stderr, "sign: --rollout must be 0-100")
		return ExitUsage
	}

	privFile, err := loadReleaseKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitError
	}
	priv, err := privFile.PrivateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitError
	}

	data, err := os.ReadFile(*artifact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign: read artifact: %v\n", err)
		return ExitError
	}

	if *minSchema == 0 {
		// Default: the schema this binary ships — the release can only
		// land on frames at or above the schema it was built with.
		migs, err := db.LoadMigrations(migrations.FS(), "frame")
		if err != nil {
			fmt.Fprintln(os.Stderr, "sign:", err)
			return ExitError
		}
		max := 0
		for _, m := range migs {
			if m.Version > max {
				max = m.Version
			}
		}
		*minSchema = max
	}

	var canaryIDs []string
	if *canary != "" {
		for _, id := range strings.Split(*canary, ",") {
			if id = strings.TrimSpace(id); id != "" {
				canaryIDs = append(canaryIDs, id)
			}
		}
	}
	now := time.Now().UTC()
	rel := updater.Release{
		Version:            *version,
		Channel:            *channel,
		OS:                 *osName,
		Arch:               *arch,
		ArtifactSHA256:     updater.ArtifactSHA256Hex(data),
		ArtifactSize:       int64(len(data)),
		MinProtocolVersion: *minProto,
		MinSchemaVersion:   *minSchema,
		ReleasedAt:         now,
		RolloutPercent:     *rollout,
		RolloutDevices:     canaryIDs,
		IsRollback:         *isRollback,
	}
	parseBound := func(name, v string, dst *time.Time) error {
		if v == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fmt.Errorf("%s: not RFC3339: %v", name, err)
		}
		*dst = t.UTC()
		return nil
	}
	if err := parseBound("--not-before", *notBefore, &rel.NotBefore); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitUsage
	}
	if err := parseBound("--expires-at", *expiresAt, &rel.ExpiresAt); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitUsage
	}

	signed, err := updater.SignRelease(rel, privFile.KeyID, priv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitError
	}
	// Self-verify before the operator trusts the file: the key and the
	// signer must obviously agree.
	pub, err := updater.DecodePublicKey(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitError
	}
	if err := updater.VerifySignature(signed, pub); err != nil {
		fmt.Fprintln(os.Stderr, "sign: self-verification failed:", err)
		return ExitError
	}

	if *out == "" {
		*out = fmt.Sprintf("openframe-%s-%s-%s-%s.json", *version, *channel, *osName, *arch)
	}
	outBytes, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitError
	}
	if err := os.WriteFile(*out, append(outBytes, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		return ExitError
	}

	fmt.Printf("signed release %s %s (%s/%s)\n", *version, *channel, *osName, *arch)
	fmt.Printf("  artifact  %s (%d bytes)\n  sha256    %s\n  signer    %s\n  rollout   %d%%%s\n  metadata  %s\n",
		*artifact, len(data), signed.ArtifactSHA256, privFile.KeyID, *rollout,
		canarySuffix(canaryIDs), *out)
	fmt.Printf("\nNext: openframe release publish --server <URL> --email <E> \\\n  --password <P> --metadata %s --artifact %s\n", *out, *artifact)
	return ExitOK
}

func canarySuffix(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf(" (canary: %s)", strings.Join(ids, ", "))
}

func loadReleaseKey(path string) (*releasePrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var k releasePrivateKey
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("parse key file: %w", err)
	}
	if k.KeyID == "" || k.Seed == "" {
		return nil, fmt.Errorf("key file %s: missing key_id or seed (is this a keygen output?)", path)
	}
	return &k, nil
}

// --- verify ---------------------------------------------------------------------

func releaseVerify(args []string) int {
	fs := flag.NewFlagSet("release verify", flag.ExitOnError)
	metaFile := fs.String("metadata", "", "signed metadata file (sign output)")
	trustFile := fs.String("trust", "", "trusted-keys file (keygen output / device trust file)")
	_ = fs.Parse(args)
	if *metaFile == "" || *trustFile == "" {
		fmt.Fprintln(os.Stderr, "verify: --metadata and --trust are required")
		return ExitUsage
	}

	b, err := os.ReadFile(*metaFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return ExitError
	}
	var rel updater.Release
	if err := json.Unmarshal(b, &rel); err != nil {
		fmt.Fprintf(os.Stderr, "verify: parse metadata: %v\n", err)
		return ExitError
	}
	tk, err := updater.LoadTrustedKeys(*trustFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return ExitError
	}
	pub, err := tk.Public(rel.SignerKeyID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: signer %q is not in the trust file (device would reject this release)\n", rel.SignerKeyID)
		return ExitError
	}
	if err := updater.VerifySignature(rel, pub); err != nil {
		fmt.Fprintln(os.Stderr, "verify: SIGNATURE INVALID:", err)
		return ExitError
	}
	fmt.Printf("OK  %s %s (%s/%s) signed by %q — signature valid against the trust file\n",
		rel.Version, rel.Channel, rel.OS, rel.Arch, rel.SignerKeyID)
	return ExitOK
}

// --- publish ---------------------------------------------------------------------

func releasePublish(args []string) int {
	fs := flag.NewFlagSet("release publish", flag.ExitOnError)
	server := fs.String("server", "", "server base URL (https://openframe.example.com)")
	email := fs.String("email", "", "admin account email")
	password := fs.String("password", "", "admin account password")
	metaFile := fs.String("metadata", "", "signed metadata file")
	artifactFile := fs.String("artifact", "", "binary to upload")
	_ = fs.Parse(args)
	for _, req := range []struct {
		name string
		v    *string
	}{{"--server", server}, {"--email", email}, {"--password", password},
		{"--metadata", metaFile}, {"--artifact", artifactFile}} {
		if *req.v == "" {
			fmt.Fprintf(os.Stderr, "publish: %s is required\n", req.name)
			return ExitUsage
		}
	}

	meta, err := os.ReadFile(*metaFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish:", err)
		return ExitError
	}
	body, err := buildReleaseUpload(meta, *artifactFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish:", err)
		return ExitError
	}

	jar, _ := cookiejar.New(nil)
	httpc := &http.Client{Jar: jar, Timeout: 10 * time.Minute}

	// Admin login (session cookie + CSRF token).
	loginBody, _ := json.Marshal(map[string]string{"email": *email, "password": *password})
	resp, err := httpc.Post(*server+"/api/v1/auth/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish: login:", err)
		return ExitError
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		fmt.Fprintf(os.Stderr, "publish: login failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(b)))
		fmt.Fprintln(os.Stderr, "publish: check --server/--email/--password")
		return ExitError
	}
	var loginOut struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginOut); err != nil || loginOut.CSRFToken == "" {
		fmt.Fprintln(os.Stderr, "publish: login response missing csrf_token")
		return ExitError
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		*server+"/api/v1/releases", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish:", err)
		return ExitError
	}
	req.Header.Set("Content-Type", body.contentType)
	req.Header.Set("X-CSRF-Token", loginOut.CSRFToken)
	resp2, err := httpc.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish: upload:", err)
		return ExitError
	}
	defer resp2.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp2.Body, 65536))
	if resp2.StatusCode != http.StatusCreated {
		fmt.Fprintf(os.Stderr, "publish: upload failed (%d): %s\n", resp2.StatusCode, strings.TrimSpace(string(out)))
		return ExitError
	}
	var uploaded struct {
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal(out, &uploaded)
	fmt.Printf("published %s (server status: %s)\n", uploaded.Version, uploaded.Status)
	if uploaded.Status == "pending" {
		fmt.Println("the release is pending — approve it in the admin UI (Releases) to make it eligible.")
	}
	return ExitOK
}

// multipartBody is a bytes.Reader + its content type, so the publish
// request can be constructed without a temp file.
type multipartBody struct {
	*bytes.Reader
	contentType string
}

func buildReleaseUpload(metadata []byte, artifactPath string) (multipartBody, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// Plain form field (not a file field): the server reads metadata
	// with FormValue, which only sees plain fields. The server also
	// accepts a file field for curl convenience.
	fw, err := w.CreateFormField("metadata")
	if err != nil {
		return multipartBody{}, err
	}
	if _, err := fw.Write(metadata); err != nil {
		return multipartBody{}, err
	}
	af, err := os.Open(artifactPath)
	if err != nil {
		return multipartBody{}, err
	}
	defer af.Close()
	fw2, err := w.CreateFormFile("artifact", filepath.Base(artifactPath))
	if err != nil {
		return multipartBody{}, err
	}
	if _, err := io.Copy(fw2, af); err != nil {
		return multipartBody{}, err
	}
	if err := w.Close(); err != nil {
		return multipartBody{}, err
	}
	return multipartBody{Reader: bytes.NewReader(buf.Bytes()), contentType: w.FormDataContentType()}, nil
}
