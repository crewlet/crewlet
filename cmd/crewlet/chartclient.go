package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// The CLI's client for /chart, and why `config import` needs one.
//
// A company FILE carries both halves — the settings and the org chart — and
// always will: an operator authors one document describing a company. The
// engine keeps them apart, because they are two things with two lifetimes,
// and `PUT /config` refuses a body carrying a chart BY NAME.
//
// So this command is where the file is DIVIDED. It is the only place that
// knows both halves came from one document, which is also what makes the
// order it writes them in a decision rather than an accident.

// chartClient talks to a running node's /chart surface.
type chartClient struct {
	base  string
	token string
	http  *http.Client
}

func newChartClient(boot *config.Bootstrap, override string) (*chartClient, error) {
	base, err := nodeBaseURL(boot, override, "the company's org chart")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken("/chart")
	if err != nil {
		return nil, err
	}
	return &chartClient{base: base, token: token, http: httpx.Client(apiTimeout)}, nil
}

// Describe names where a write lands, for the line the command prints.
func (c *chartClient) Describe() string {
	return "the company's org chart, through " + c.base +
		" (one record per object, applied on every node)"
}

// ImportStructure publishes one revision's complete authored placement.
//
// KEYED ON THE REVISION, which is what makes a re-import a no-op every node
// reaches the same way: re-activating an unchanged revision is the
// credential-rotation gesture and is therefore routine, and without the
// ledger it would rewrite every row in the chart and wake everybody again.
func (c *chartClient) ImportStructure(ctx context.Context, revision string,
	edges []chart.Edge) (string, error) {

	type edgeBody struct {
		Object struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"object"`
		Parent string `json:"parent,omitempty"`
		Lead   string `json:"lead,omitempty"`
	}
	out := make([]edgeBody, 0, len(edges))
	for _, e := range edges {
		var one edgeBody
		one.Object.Kind, one.Object.ID = string(e.Object.Kind), e.Object.ID
		one.Parent, one.Lead = e.Parent, e.Lead
		out = append(out, one)
	}
	body, err := json.Marshal(map[string]any{"revision": revision, "edges": out})
	if err != nil {
		return "", err
	}
	answer, err := c.write(ctx, http.MethodPost, "/chart/import", body)
	if err != nil {
		return "", err
	}
	return answer.Position, nil
}

// WriteUnit and WriteSeat publish one object's content.
//
// ONE RECORD PER OBJECT, on that object's own subject, which is what the
// import record deliberately does not carry: a chart of five hundred seats at
// this domain's prose bound is megabytes, and an external NATS cluster's
// default max_payload is one mebibyte — so an import that carried content
// would be refused by the broker on exactly the companies large enough to
// need it.
func (c *chartClient) WriteUnit(ctx context.Context, unit chart.AuthoredUnit) error {
	body, err := json.Marshal(map[string]any{
		"name": unit.Name, "type": unit.Type, "purpose": unit.Purpose,
		"goals": unit.Goals, "channel": unit.Channel,
		"project": unit.Project, "space": unit.Space,
		"knowledge_refs": unit.KnowledgeRefs,
		"runtime":        json.RawMessage(unit.Runtime),
	})
	if err != nil {
		return err
	}
	_, err = c.write(ctx, http.MethodPatch, "/chart/units/"+unit.Key, body)
	return err
}

func (c *chartClient) WriteSeat(ctx context.Context, seat chart.AuthoredSeat) error {
	body, err := json.Marshal(map[string]any{
		"kind": string(seat.Kind),
		// THE UNIT THE STRUCTURE JUST PLACED IT IN. The domain refuses a
		// value that disagrees with the row, so this is what makes the
		// order of the two writes load-bearing: the placement lands
		// first, and the content states the placement it can now see.
		"unit": seat.Unit,
		"name": seat.Name, "email": seat.Email,
		"backstory": seat.Backstory, "goal": seat.Goal,
		"responsibilities":      seat.Responsibilities,
		"behavioral_guidelines": seat.BehavioralGuidelines,
		"manages":               seat.Manages,
		"project":               seat.Project, "space": seat.Space,
		"runtime": json.RawMessage(seat.Runtime),
	})
	if err != nil {
		return err
	}
	_, err = c.write(ctx, http.MethodPatch, "/chart/seats/"+seat.Handle, body)
	return err
}

// chartAnswer is what every write on this surface reports about itself.
type chartAnswer struct {
	Outcome  string `json:"outcome"`
	Position string `json:"position"`
	OpID     string `json:"op_id"`
	Detail   string `json:"detail"`
}

// write performs one request and reads its answer.
//
// A `202` IS NOT AN ERROR AND IS NOT A SUCCESS EITHER. The record is durable
// and this node has not applied it, so the next read HERE may not see it —
// which for an import means the content write that follows may be arbitrated
// against a placement this node cannot yet read. The caller decides; this
// reports.
func (c *chartClient) write(ctx context.Context, method, path string, body []byte) (
	chartAnswer, error) {

	req, err := http.NewRequestWithContext(ctx, method, c.base+path,
		bytes.NewReader(body))
	if err != nil {
		return chartAnswer{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return chartAnswer{}, fmt.Errorf("reaching %s: %w\n\nthe node's API is "+
			"what publishes a chart record; check it is running and that -api "+
			"names its address", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponseBytes))
	if err != nil {
		return chartAnswer{}, fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if resp.StatusCode/100 != 2 {
		return chartAnswer{}, chartRefusal(method+" "+path, resp.StatusCode, raw)
	}
	var answer chartAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return chartAnswer{}, fmt.Errorf("the node answered %s %s with something "+
			"this build cannot read: %w", method, path, err)
	}
	return answer, nil
}

// chartRefusal renders what the node said, rather than the status alone.
//
// THE DETAIL IS THE MESSAGE. Every refusal on this surface names the rule it
// broke — a seat that moved, an address somebody holds, a grant the caller
// does not carry — and a client that printed "400" would throw away the one
// sentence that says what to do.
func chartRefusal(what string, status int, raw []byte) error {
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Reason string `json:"reason"`
		Hint   string `json:"hint"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := body.Detail
	if msg == "" {
		msg = body.Reason
	}
	if msg == "" {
		msg = string(raw)
	}
	out := fmt.Sprintf("%s was refused (%d): %s", what, status, msg)
	if body.Hint != "" {
		out += "\n\n" + body.Hint
	}
	return fmt.Errorf("%s", out)
}
