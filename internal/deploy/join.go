package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
)

// Join gathers what a SECOND device needs to reach an existing bus: the
// endpoint, read from the stack, and the bearer token, read from this
// machine's own token file.
//
// The token cannot come from AWS. It is a NoEcho stack parameter, so
// CloudFormation will not hand it back, and that is not an obstacle to route
// around — it is the design. The premise of this backend is that a device
// needs no AWS credentials, which means no device can ever fetch its own
// credential from AWS; something has to carry the secret across, and that
// something is a person. What this function can do is make that hop one
// deliberate step with a way to check it landed, instead of an improvised
// copy nobody verifies.
func Join(ctx context.Context, o Options) (*JoinInfo, error) {
	if o.Stack == "" {
		o.Stack = "muster"
	}
	if o.TokenFile == "" {
		return nil, errors.New("no token file to read; pass -token-file")
	}
	raw, err := os.ReadFile(o.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token from %s (run this on the machine you deployed from): %w", o.TokenFile, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return nil, fmt.Errorf("token file %s is empty", o.TokenFile)
	}

	var loadOpts []func(*config.LoadOptions) error
	if o.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(o.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	outs, err := stackOutputs(ctx, cloudformation.NewFromConfig(cfg), o.Stack)
	if err != nil {
		return nil, err
	}
	endpoint := outs["MusterUrl"]
	if endpoint == "" {
		return nil, fmt.Errorf("stack %s has no MusterUrl output; is it the muster backend?", o.Stack)
	}
	return &JoinInfo{
		Endpoint:    endpoint,
		Token:       tok,
		Fingerprint: Fingerprint(tok),
		TokenPath:   o.TokenFile,
	}, nil
}

// JoinInfo is everything needed to add one device to an existing bus.
type JoinInfo struct {
	Endpoint    string
	Token       string
	Fingerprint string
	TokenPath   string
}

// WriteInstructions renders the paste-ready setup for the machine being added.
//
// The commands and the token are deliberately SEPARATE. Inlining the secret
// into the pasted block would put it in the new machine's shell history, on a
// machine whose history the operator may not think about; the `read -rs` form
// keeps it out of history and off the screen, at the cost of one extra paste.
// The token is still printed here, on the machine that already has it, because
// there is no other way to move it — but printing it once where it already
// lives is different from writing it into a second machine's history forever.
func (j *JoinInfo) WriteInstructions(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\nAdd a device to this bus. On the OTHER machine, run:\n\n")
	p("  export MUSTER_BACKEND=remote\n")
	p("  export MUSTER_REMOTE_URL=%s\n", j.Endpoint)
	p("  mkdir -p %s\n", filepath.Dir(j.TokenPath))
	p("  ( umask 077; read -rs TOKEN && printf '%%s' \"$TOKEN\" > %s )\n\n", j.TokenPath)
	p("Paste this at the blank line (it is not in the block above, so it does\n")
	p("not land in that machine's shell history):\n\n")
	p("  %s\n\n", j.Token)
	p("Then confirm both machines hold the same token — this compares a hash,\n")
	p("not the secret, so it is safe to read out loud:\n\n")
	p("  tr -d '\\n' < %s | shasum -a 256 | cut -c1-16\n", j.TokenPath)
	p("  expected: %s\n\n", j.Fingerprint)
	p("Anyone who sees that token has full access to the bus: every message,\n")
	p("and the ability to post as any agent. Clear this screen when you're done.\n")
}
