package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// The CLI's client for /config, and why the command needs one.
//
// `crewlet config` opens the node's store file directly, which works only
// while the engine is STOPPED: the store is exclusive to one process, so an
// import against a running node was refused outright and the operator was
// told to go and write the curl by hand. That left the one gesture a fleet
// needs most — "make this file the company, now, everywhere" — with no CLI at
// all, while `crewlet secrets` had had exactly that route for its own estate
// all along.
//
// # It is not a fallback, it is the primary path for a running fleet
//
// An offline import marks a revision active in THIS node's database and
// cannot move the fleet's activation pointer, because on the default topology
// that pointer lives inside the engine's own process. PUT /config stores the
// revision AND activates it, so every node converges on it with no restart.

// configClient talks to a running node's /config surface.
type configClient struct {
	base  string
	token string
	http  *http.Client
}

func newConfigClient(boot *config.Bootstrap, override string) (*configClient, error) {
	base, err := nodeBaseURL(boot, override, "the fleet's company configuration")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken(boot, "/config")
	if err != nil {
		return nil, err
	}
	return &configClient{base: base, token: token, http: httpx.Client(apiTimeout)}, nil
}

// Describe names where a write lands, for the line the command prints.
func (c *configClient) Describe() string {
	return "the fleet's company configuration, through " + c.base +
		" (every node converges on it, with no restart)"
}

// Import sends a company document as a new active revision.
//
// THE FILE'S OWN BYTES, not a re-encoding of the parsed document: Tier B's
// secrets are `${VAR}` POINTERS stored verbatim, and a round trip through the
// Go types would be a second opinion about a document the node is about to
// form its own. The caller validates first so a typo is caught here rather
// than after a round trip, but what travels is what the operator wrote.
func (c *configClient) Import(ctx context.Context, doc []byte, summary string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.base+"/config", bytes.NewReader(doc))
	if err != nil {
		return "", 0, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/yaml")
	// REQUIRED BY THE ROUTE, and rightly: the revision history is the
	// record of who changed what and why, and a write with no reason in it
	// is the one an operator is looking at six months later.
	req.Header.Set("X-Summary", summary)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("reaching %s: %w\n\nthe node's API is what "+
			"activates a revision fleet-wide; check it is running and that "+
			"-api names its address", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponseBytes))
	if err != nil {
		return "", 0, fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if resp.StatusCode/100 != 2 {
		return "", 0, c.refusal(resp.StatusCode, raw)
	}
	var body struct {
		RevisionID string `json:"revision_id"`
		Epoch      int64  `json:"epoch"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", 0, fmt.Errorf("the node answered PUT /config with something "+
			"this build cannot read: %w", err)
	}
	return body.RevisionID, body.Epoch, nil
}

// maxConfigResponseBytes bounds one answer. Every response this client reads
// is a small JSON object — a revision id and an epoch, or a refusal and its
// hint.
const maxConfigResponseBytes = 64 << 10

// refusal turns a status code into something an operator can act on.
func (c *configClient) refusal(status int, raw []byte) error {
	var body struct {
		Error   string `json:"error"`
		Detail  string `json:"detail"`
		Hint    string `json:"hint"`
		Current string `json:"current_revision_id"`
		Stored  string `json:"stored_revision_id"`
	}
	_ = json.Unmarshal(raw, &body)
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%s has no /config surface: it is running a build "+
			"from before this route existed, or it is not an engine node", c.base)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s refused the bearer token: set %s to one of its "+
			"api.auth.tokens", c.base, apiTokenEnv)
	case status == http.StatusConflict && body.Error == "revision_advanced":
		// STORED BUT NOT ACTIVATED, which is a recoverable state and has
		// to be said as one: the operator's document is safe in the
		// history, and one command puts it live.
		return fmt.Errorf("another write activated first, so this document was "+
			"stored as revision %s but is NOT active (the fleet is on %s).\n"+
			"Review the difference and activate it if it is still what you "+
			"want:\n  crewlet config diff %s\n  crewlet config activate %s",
			body.Stored, body.Current, body.Stored, body.Stored)
	}
	msg := body.Error
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	for _, extra := range []string{body.Detail, body.Hint} {
		if extra != "" {
			msg += "\n  " + extra
		}
	}
	return fmt.Errorf("%s answered %d for PUT /config: %s", c.base, status, msg)
}
