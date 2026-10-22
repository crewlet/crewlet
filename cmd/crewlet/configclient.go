package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
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
// An offline import only OFFERS a revision: this node's store boots on it and
// the next start publishes it, and only over the revision it was built on —
// it cannot move the fleet's activation pointer, which on the default
// topology lives inside the engine's own process. PUT /config stores the
// revision AND activates it, so every node converges on it with no restart.

// configClient talks to a running node's /config surface.
type configClient struct {
	base  string
	token string
	http  *http.Client
	// answerCap is the most of one answer read back: [maxConfigResponseBytes]
	// in every client [newConfigClient] builds. A field so the suite can send
	// an answer past it without building one of 64 MiB; zero is refused by
	// [configClient.Import] rather than read as a cap that refuses every
	// answer.
	answerCap int
}

func newConfigClient(environ config.Source, boot *config.Bootstrap, override string) (*configClient, error) {
	base, err := nodeBaseURL(boot, override, "the fleet's company configuration")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken(environ, "/config")
	if err != nil {
		return nil, err
	}
	return &configClient{
		base: base, token: token, http: httpx.Client(apiTimeout),
		answerCap: maxConfigResponseBytes,
	}, nil
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
	if c.answerCap <= 0 {
		return "", 0, fmt.Errorf("the /config client for %s has no answer cap, so it could "+
			"read no answer at all; it is built by newConfigClient", c.base)
	}
	// AS AN IMPORT, which is what this is: a restored or migrated file, whose
	// charter faults the node warns about rather than refuses, as an offline
	// import and a boot import do (configapi's mayImport). Judged as an edit,
	// a file restoring an older charter was refused for a part it changed back.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.base+"/config?import=true", bytes.NewReader(doc))
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
	// +1 SO THE OVERRUN IS VISIBLE. io.LimitReader stops at its cap with a
	// clean end of file, so an answer past it reached the decoder clipped
	// and was reported as something this build cannot read — a protocol
	// fault that is not there, on a write that may well have landed.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.answerCap)+1))
	if err != nil {
		return "", 0, fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if len(raw) > c.answerCap {
		landed := "so whether the revision was stored is unknown"
		if resp.StatusCode/100 == 2 {
			landed = fmt.Sprintf("though its status, %d, says the revision was stored", resp.StatusCode)
		}
		return "", 0, fmt.Errorf("the answer from %s to PUT /config exceeded %d bytes, so it was "+
			"not read, %s: no company's answer comes near the cap, so check that -api names "+
			"the engine rather than something in front of it", c.base, c.answerCap, landed)
	}
	if resp.StatusCode/100 != 2 {
		return "", 0, c.refusal(resp.StatusCode, resp.Header.Get("Content-Type"), raw)
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
// SIZED FOR THE DOCUMENT, not for the two fields this client reads. A write's
// answer carries a located warning per thing the engine will run but a person
// should know about, and a refusal a located problem per failure, so an
// answer grows with the document it judged. The 64 KiB this used to be lost a
// refusal's detail on a large document, and reported a write that had landed
// as an answer this build could not read.
//
// Sixteen times the largest document the route accepts
// ([configapi.MaxBodyBytes]), because each entry restates its path, its
// segments and a sentence saying what to write instead, which is routinely
// ten times the field it is about — a mistyped provider key is a few bytes and
// its problem a few hundred. It still bounds a node that answers with far
// more than any document could produce, and the client's own timeout bounds
// one that never stops.
const maxConfigResponseBytes = 16 * configapi.MaxBodyBytes

// refusal turns a status code into something an operator can act on.
func (c *configClient) refusal(status int, contentType string, raw []byte) error {
	var body struct {
		refusalWords
		Error   string `json:"error"`
		Reason  string `json:"reason"`
		Current string `json:"current_revision_id"`
		Stored  string `json:"stored_revision_id"`
		// Node is the node whose history alone holds Stored, which the
		// engine names beside it on every answer that names one.
		Node    string `json:"node"`
		Outcome string `json:"outcome"`
	}
	_ = json.Unmarshal(raw, &body)
	if msg, ok := credentialRefusal(status, raw, true); ok {
		return fmt.Errorf("%s: %s", c.base, msg)
	}
	switch {
	case status == http.StatusNotFound:
		// PUT /config answers no 404 of its own, so any 404 is the surface
		// missing rather than something under it.
		return missingSurface(c.base, "/config", body.Error)
	case status == http.StatusConflict && body.Error == "revision_advanced":
		return lostRace(c.base, body.Current, body.Stored, body.Node)
	case status == http.StatusServiceUnavailable && body.Outcome == httpjson.OutcomeUnknown:
		return unansweredImport(c.base, body.Stored, body.Node)
	case status == http.StatusServiceUnavailable && body.Error == string(httpjson.CodeUnavailable) &&
		body.Reason == "" && body.Detail != "":
		// ANY OTHER CONDITION OF THE NODE — a store too busy to take the
		// revision, one closed for a snapshot, a log at its ceiling — is told
		// in the node's own words for a person, which say whether waiting
		// clears it: blaming every 503 on a leader election sent an operator
		// waiting for one that was never coming.
		return fmt.Errorf("%s could not take the import, so nothing was written. %s",
			c.base, body.Detail)
	case status == http.StatusServiceUnavailable && body.Error == string(httpjson.CodeUnavailable):
		// THE FLEET'S POINTER COULD NOT BE READ, which every write is built
		// on: nothing was written, and it clears within a leader election.
		// Said as that, because "answered 503: unavailable" reads as a node
		// that is down, and the gesture is the same import a moment later.
		// A write the node STORED and could not activate is the unknown
		// outcome above, which this is never said of.
		reason := body.Reason
		if reason == "" {
			reason = "the node could not read the fleet's activation"
		}
		return fmt.Errorf("%s could not take the import yet, so nothing was written: %s.\n"+
			"  It clears within a few seconds (a coordination leader election); run this "+
			"import again", c.base, reason)
	case status == http.StatusForbidden && body.Error == string(httpjson.CodeConfigManaged):
		// THE CREDENTIAL IS VALID AND NOT A WRITER, so the remedy is which
		// token this command sent, and the refusal says where it came from.
		return fmt.Errorf("%s refused the import: %s\n  %s\n  (this command "+
			"signs in with %s)",
			c.base, body.Detail, body.Hint, apiTokenEnv)
	}
	msg := body.Error
	if msg == "" {
		msg = foreignAnswer(contentType, raw)
	}
	return fmt.Errorf("%s answered %d for PUT /config: %s", c.base, status,
		withRefusalDetail(msg, body.refusalWords))
}

// unansweredImport is an import the node stored and whose activation the
// coordination store never answered: it may be what the fleet runs already, so
// it is never said to have written nothing, and running the import again —
// which makes another revision, over this one if it landed — is not the remedy.
// Looking is: the active revision either is the stored one or is not.
//
// AND WHERE TO LOOK, which is one node. The revision is in the history of the
// node that stored it and of no other until an activation names it, so the
// diff and the revert that make it live answer there alone; -api naming a
// load balancer or a shared Service in front of several reaches another,
// which answers 404, and the operator concludes the import is lost. The answer
// names that node, and so does this.
func unansweredImport(base, stored, node string) error {
	if stored == "" {
		return fmt.Errorf("%s stored this document and could not tell whether the fleet "+
			"activated it.\nRead %s/config once the node answers again to see which "+
			"revision is live before running this import again", base, base)
	}
	return fmt.Errorf("%s stored this document as revision %s and could not tell "+
		"whether the fleet activated it: the coordination store did not answer.\n"+
		"Once it answers again, GET %s/config names the live revision. If it is %s, "+
		"the import landed; if it is not, make it live through %s:\n"+
		"  GET  %s/config/revisions/%s/diff\n"+
		"  POST %s/config/revisions/%s/revert\n"+
		"Running this import again makes another revision, over this one if it landed",
		base, stored, base, stored, holderOf(node), base, stored, base, stored)
}

// holderOf names the node whose history alone holds a stored revision, and
// what reaching it takes: -api may name an address several nodes share, and
// every node but this one answers 404 for the revision.
func holderOf(node string) string {
	if node == "" {
		return "the node that answered, which alone holds it"
	}
	return "node " + node + ", which alone holds it — by its own address if -api " +
		"names one several nodes share, since any other answers 404 for it"
}

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
// activate an empty id. And what was stored is one node's ([holderOf]), as an
// unanswered import's is: a revision that lost its activation reached no other.
func lostRace(base, current, stored, node string) error {
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
		"want, through %s:\n"+
		"  GET  %s/config/revisions/%s/diff\n"+
		"  POST %s/config/revisions/%s/revert",
		stored, fleet, holderOf(node), base, stored, base, stored)
}
