package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/textcut"
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
	// +1 SO THE OVERRUN IS VISIBLE, and REFUSED at the cap rather than read
	// up to it — the rule this client's three siblings in this directory
	// each write down and this one did not keep. io.LimitReader reports a
	// clean EOF when it stops, so a clipped answer reached json.Unmarshal as
	// malformed JSON and was reported as "not the expected JSON": an
	// activation that LANDED on the node came back looking like a protocol
	// fault, and the operator re-ran an import that had already succeeded.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponseBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if len(raw) > maxConfigResponseBytes {
		return "", 0, fmt.Errorf(
			"the answer from %s exceeded %d bytes, so it was not read: this "+
				"build caps one answer to bound a proxy's error page. Check "+
				"that -api names the engine rather than something in front of "+
				"it. The revision may still have been activated — run "+
				"`crewlet config show` before importing again",
			c.base, maxConfigResponseBytes)
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

// maxConfigResponseBytes bounds one answer.
//
// SIZED FOR THE COMPANY, not for the two fields this client reads. A write's
// answer carries the engine's derived hierarchy of the whole document it
// stored, every seat with its managers and reports, and a refusal carries a
// located problem per failure beside the same hierarchy, so an answer grows
// with the company it describes. The 64 KiB this used to be is about two
// hundred seats of hierarchy: past that, a write that had landed was reported
// as an answer this build could not read, and a refusal lost its detail.
//
// Sixteen times the largest document the route accepts
// ([configapi.MaxBodyBytes]), because the hierarchy restates each seat of the
// document with its relations spelled out, and that restatement is several
// times the size of a seat as it is usually written. It still bounds a node
// that answers with far more than any company could produce, and the client's
// own timeout bounds one that never stops.
const maxConfigResponseBytes = 16 * configapi.MaxBodyBytes

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
		return lostRace(c.base, body.Current, body.Stored)
	}
	msg := body.Error
	if msg == "" {
		// NOT THE ENGINE'S JSON, so a proxy's page or a plain-text error,
		// shown for a person to recognise rather than read whole: an
		// answer can be as large as maxConfigResponseBytes.
		msg = textcut.Ellipsis(strings.TrimSpace(string(raw)), maxRefusalTextBytes)
	}
	return fmt.Errorf("%s answered %d for PUT /config: %s", c.base, status,
		withRefusalDetail(msg, body.Detail, body.Hint))
}

// maxRefusalTextBytes is how much of an answer that is not the engine's JSON
// a refusal quotes: a screenful, enough to tell a proxy's error page from a
// node's plain-text one.
const maxRefusalTextBytes = 2 << 10

// lostRace is what an import refused with revision_advanced tells the
// operator: what won, whether their document was kept, and what puts it live
// from where they are.
//
// FROM WHERE THEY ARE, which is talking to a running node: this route is taken
// because the engine holds its store, or because -api names a node elsewhere.
// `crewlet config diff` and `crewlet config activate` open that store
// directly, so the commands this used to suggest refused on exactly the node
// that had just answered. The node's own routes are what work: the revision
// endpoints to look at what was kept, a revert to make it live, and a second
// import to write the file again over what won.
//
// NOTHING STORED IS ITS OWN ANSWER. A write refused before it stored anything
// names no revision, and a hint built around one sent the operator to
// activate an empty id.
func lostRace(base, current, stored string) error {
	fleet := "a newer revision"
	if current != "" {
		fleet = "revision " + current
	}
	if stored == "" {
		return fmt.Errorf("another write activated first, so nothing was stored "+
			"(the fleet is on %s).\nRead %s/config to see what is live, and run "+
			"this import again if this document should replace it", fleet, base)
	}
	return fmt.Errorf("another write activated first, so this document was "+
		"stored as revision %s but is NOT active (the fleet is on %s).\n"+
		"Compare it with what is live, and make it live if it is still what you "+
		"want, through the node that answered:\n"+
		"  GET  %s/config/revisions/%s/diff\n"+
		"  POST %s/config/revisions/%s/revert",
		stored, fleet, base, stored, base, stored)
}
