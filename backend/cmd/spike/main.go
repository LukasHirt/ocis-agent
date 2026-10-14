// Command spike is a throwaway probe for the open questions Q3 and Q4 and for
// the WebDAV conditional-request behaviour the agent's write tools rely on. It
// prints a markdown report on stdout. Never print a token: everything goes
// through redact().
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		base      = flag.String("base", "https://localhost:9200", "oCIS base URL (through the dev proxy)")
		user      = flag.String("user", "einstein", "oCIS user")
		pass      = flag.String("pass", "relativity", "oCIS user password (needs PROXY_ENABLE_BASIC_AUTH=true)")
		insecure  = flag.Bool("insecure", true, "skip TLS verification (dev stack uses a self-signed cert)")
		sseWindow = flag.Duration("sse-window", 15*time.Second, "how long to listen for SSE events per run")
	)
	flag.Parse()

	if err := run(*base, *user, *pass, *insecure, *sseWindow); err != nil {
		fmt.Fprintln(os.Stderr, "spike:", err)
		os.Exit(1)
	}
}

func run(base, user, pass string, insecure bool, window time.Duration) error {
	ctx := context.Background()
	c := newClient(strings.TrimRight(base, "/"), insecure)
	out := os.Stdout
	owner := cred{user, pass}

	fmt.Fprintf(out, "# Spike report %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "Target: %s as `%s`\n\n", base, user)

	resp, tok, err := c.createToken(ctx, owner, "ocis-agent-spike")
	if err != nil {
		return fmt.Errorf("create first token: %w", err)
	}
	if tok == nil {
		return fmt.Errorf("create first token: status %d body %q (is auth-app running and PROXY_ENABLE_APP_AUTH=true?)",
			resp.status, redact(string(resp.body), pass))
	}
	app := cred{user, tok.Token}
	defer func() {
		_, _ = c.deleteByLabel(ctx, owner, "ocis-agent-spike")
		_, _ = c.deleteByLabel(ctx, owner, "ocis-agent-spike-renewed")
	}()

	fmt.Fprintln(out, "## Setup")
	fmt.Fprintf(out, "- POST /auth-app/tokens authenticated with the user password: **%d** (200 is expected: the handler writes the body before WriteHeader(201))\n", resp.status)
	fmt.Fprintf(out, "- token expires %s, label %q\n", tok.ExpirationDate.Format(time.RFC3339), tok.Label)
	meResp, meErr := c.me(ctx, app)
	fmt.Fprintf(out, "- GET /graph/v1.0/me with the app token (Basic auth): %s\n", status(meResp, meErr))
	if meErr != nil || meResp.status != http.StatusOK {
		return fmt.Errorf("the new app token does not authenticate against Graph (status %s); cannot continue", status(meResp, meErr))
	}
	driveID, err := c.personalDriveID(ctx, app)
	if err != nil {
		return fmt.Errorf("find personal drive: %w", err)
	}
	fmt.Fprintf(out, "- personal drive id: `%s`\n\n", driveID)

	fmt.Fprintln(out, "## Q4: SSE file events")
	probeSSE(ctx, c, "app token (Basic auth)", app, driveID, window, out)
	probeSSE(ctx, c, "user password (control)", owner, driveID, window, out)

	probeConditional(ctx, c, app, driveID, out)

	// Q3 runs last: it deletes the original token when renewal works, which
	// would break the probes above.
	probeQ3(ctx, c, app, tok, out)
	return nil
}
