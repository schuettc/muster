package deploy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Repo is the GitHub repository release artifacts are fetched from. A
// self-hoster building their own fork overrides this with -repo rather than
// editing it, so the default can stay the canonical upstream.
const Repo = "schuettc/muster"

// maxArtifactBytes caps a downloaded Lambda zip. The real artifact is ~7MB;
// this bounds what a redirected or hijacked URL can write to disk and to
// memory before anything looks at it.
const maxArtifactBytes = 64 << 20

// ArtifactName is the Lambda zip's filename for a release tag. It matches
// .github/workflows/release.yml exactly — the name is version-stamped
// because CloudFormation only redeploys Lambda code when S3Key (or
// S3ObjectVersion) CHANGES. Overwriting one fixed key and re-running deploy
// leaves the OLD binary running with no error anywhere, so the version lives
// in the key and upgrades are correct by default.
func ArtifactName(tag string) string {
	return "muster-lambda-arm64-" + tag + ".zip"
}

// ArtifactURL is the release download URL for a tag's Lambda zip.
func ArtifactURL(repo, tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, ArtifactName(tag))
}

// ReleaseTag turns a version stamp into its release tag. Release tags carry a
// leading "v" and the VERSION file does not, so exactly one place converts
// between them.
func ReleaseTag(version string) string {
	if version == "" {
		return ""
	}
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

// BucketName is the default artifact bucket for an account: muster-artifacts-
// plus the account id. S3 bucket names are globally unique across all of AWS,
// so a fixed name would collide with every other operator's; the account id
// is the shortest thing that is both unique to the operator and derivable
// without asking them for one more parameter.
func BucketName(accountID string) string {
	return "muster-artifacts-" + accountID
}

// GenerateToken mints a bearer token: 32 bytes from crypto/rand, base64. This
// is the ONLY thing standing between the internet and the whole bus (the
// endpoint has no authorizer), so it is never derived from a hostname, a
// timestamp, or anything else guessable — entropy is the entire security
// argument, and the template's 32-character minimum is a floor rather than a
// substitute for it.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// WriteToken writes tok to path with mode 0600, creating parent directories.
// The mode is set through the open rather than a following chmod so the file
// is never briefly readable by others — the daemon refuses to start on a
// looser mode, and a window where it is loose is a window where something
// could have read it.
func WriteToken(path, tok string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, tok); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Fingerprint is a short, non-reversible identifier for a token: the first 16
// hex characters of its SHA-256. Two machines can compare fingerprints in the
// clear to confirm they hold the SAME token, which is the only way to verify a
// hand-copied secret without either party revealing it. Truncated because the
// job is catching a typo or a truncated paste, not resisting a preimage
// attack on a value both sides already have.
func Fingerprint(tok string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return hex.EncodeToString(sum[:])[:16]
}

// download fetches url into a temporary file and returns its path plus a
// cleanup func. It streams to disk rather than into memory: the caller hands
// the result straight to S3, which wants an io.Reader it can seek for the
// checksum anyway.
func download(url string) (path string, cleanup func(), err error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 404 is the overwhelmingly likely failure and it has one cause worth
		// naming, because the fix is not obvious from "404": the release
		// exists but has no Lambda zip, or the tag does not exist at all.
		return "", nil, fmt.Errorf("download %s: HTTP %d (does that release exist, and does it carry a Lambda artifact?)",
			url, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "muster-lambda-*.zip")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxArtifactBytes))
	if err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("download %s: %w", url, err)
	}
	if n == maxArtifactBytes {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("download %s: artifact exceeds %d bytes", url, int64(maxArtifactBytes))
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}
