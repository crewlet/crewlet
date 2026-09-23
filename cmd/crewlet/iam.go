package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// `crewlet iam` — the company's identity directory from a terminal.
//
// # Why it goes through a running node and never at the database
//
// The directory is a state-log domain: a change is a RECORD, arbitrated at
// the broker on the subject of the thing it changes, and the broker is
// embedded in the engine's own process with no listener. A second process
// cannot publish one. So this speaks to `/iam` on a running node, exactly as
// `crewlet secrets` speaks to `/secrets` and for the same reason.
//
// # And why every write answers three ways
//
// `applied` means this node has the change and the next read here sees it;
// `pending` means the record is durable and this node has not applied it yet,
// and carries the position to read at; `unknown` means nothing can be
// established from here and the only safe retry is the SAME operation id.
// Every command prints which, because a caller that read all three as success
// would tell somebody a revocation landed when nothing can say it did.

const iamUsage = `crewlet iam — the company's people, credentials and sessions

Usage:
  crewlet iam people [-q TERM] [-stage S] [-limit N]   The directory
  crewlet iam show ID                                  One person, in full
  crewlet iam invite EMAIL [-grants G,...] [-colleague L]
                                                       Issue a link, shown ONCE
  crewlet iam create [-login L] [-email E] [-kind K]   Create somebody directly
  crewlet iam bind ID SEAT                             Bind a person to a chart seat
  crewlet iam unbind ID                                Take the binding back
  crewlet iam grant ID [-grants G,...] [-colleague L]  Change what somebody carries
  crewlet iam suspend ID                               Stop them acting, keep the row
  crewlet iam activate ID                              Let them act again
  crewlet iam remove ID                                Tombstone them and destroy their key
  crewlet iam revoke ID                                End every session and token they hold
  crewlet iam sessions ID                              Their sessions, newest first
  crewlet iam credentials [-person ID]                 What somebody proves themselves with
  crewlet iam token [-person ID] [-label L] [-days N]  Mint a machine token, shown ONCE
  crewlet iam revoke-credential CREDENTIAL_ID [-person ID]
                                                       Withdraw one credential
  crewlet iam reset-mfa ID                             Clear the second factor and end sessions
  crewlet iam invalidate-all                           Invalidate EVERY session in the company
  crewlet iam bootstrap-code                           Re-issue the one-time founder code
  crewlet iam check                                    What is wrong with this company's access
  crewlet iam audit [-person ID] [-event OP] [-since POSITION] [-at TIME]
                                                       The identity estate's own trail

Flags:
  -config PATH   Tier A config naming the node to reach (default %q)
  -api URL       The running node's API; default is its own api.host:port
  -json          Print the raw answer rather than a table
  -reason TEXT   Recorded on the change, and read by whoever audits it

Export CREWLET_API_TOKEN to authenticate. Every /iam route is guarded, reads
included: a map of who can reach a company is worth as much as the grants.
`

// runIAM dispatches `crewlet iam`.
func runIAM(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, iamUsage, defaultBootstrapPath)
		return flag.ErrHelp
	}
	// THE SUBJECT COMES OFF BEFORE THE FLAGS, for `crewlet secrets`'
	// reason: `iam show ID -json` is the natural spelling and Go's flag
	// package stops at the first non-flag argument, so parsing first
	// would leave every flag after the id unparsed and silently
	// defaulted.
	subject, rest := splitSubject(rest)
	second, rest := splitSubject(rest)

	fs := flag.NewFlagSet("iam "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bootstrapPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: the node this command reaches")
	apiURL := fs.String("api", "",
		"the running node's API; default is the api.host:port in -config")
	asJSON := fs.Bool("json", false, "print the raw answer")
	reason := fs.String("reason", "", "recorded on the change")
	grants := fs.String("grants", "",
		"a comma-separated grant list, or `none` for an empty one")
	colleague := fs.String("colleague", "",
		"reach into the company's work: none, read or write")
	stage := fs.String("stage", "", "narrow the directory to one enrolment stage")
	term := fs.String("q", "", "narrow the directory on a login or a seat")
	limit := fs.Int("limit", 0, "how many rows at most")
	login := fs.String("login", "", "the login to create somebody under")
	email := fs.String("email", "", "the address to create somebody under")
	kind := fs.String("kind", "", "person or machine (create only)")
	name := fs.String("name", "", "the person's own name")
	person := fs.String("person", "", "whose credentials or sessions")
	label := fs.String("label", "", "what to call a minted token")
	days := fs.Int("days", 0, "how long a minted token lasts")
	event := fs.String("event", "", "narrow the trail to one operation")
	since := fs.Uint64("since", 0, "the oldest log POSITION to include")
	at := fs.String("at", "", "an RFC 3339 instant, resolved to a position once")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// EnvOnly, because Tier A is the root of trust: it holds the keys to
	// the secret store, so it can never read a value out of it.
	// THE SUBJECT IS CHECKED BEFORE THE CONFIG IS READ. An operator who
	// typed `crewlet iam show` needs to be told they left off a person id
	// — not that their config file is missing, which is a different
	// problem they do not have.
	if err := iamSubjects.check(sub, subject, second); err != nil {
		return err
	}
	boot, err := config.LoadBootstrap(*bootstrapPath, config.EnvOnly())
	if err != nil {
		return fmt.Errorf("read %s: %w", *bootstrapPath, err)
	}
	client, err := newIAMClient(boot, *apiURL)
	if err != nil {
		return err
	}
	ctx := context.Background()
	out := &iamPrinter{w: stdout, raw: *asJSON}

	switch sub {
	case "people":
		query := url.Values{}
		setIf(query, "q", *term)
		setIf(query, "stage", *stage)
		if *limit > 0 {
			query.Set("limit", strconv.Itoa(*limit))
		}
		return out.people(client.get(ctx, "/iam/people", query))
	case "show":
		return out.one(client.get(ctx, "/iam/people/"+subject, nil))
	case "invite":
		body, err := iamGrantsBody(*grants, *colleague)
		if err != nil {
			return err
		}
		body["email"] = subject
		body["reason"] = *reason
		return out.invite(client.post(ctx, "/iam/invitations", body))
	case "create":
		body, err := iamGrantsBody(*grants, *colleague)
		if err != nil {
			return err
		}
		body["login"], body["email"] = *login, *email
		body["name"], body["kind"] = *name, *kind
		body["reason"] = *reason
		return out.written(client.post(ctx, "/iam/people", body))
	case "bind":
		return out.written(client.patch(ctx, "/iam/people/"+subject,
			map[string]any{"seat": second, "reason": *reason}))
	case "unbind":
		return out.written(client.patch(ctx, "/iam/people/"+subject,
			map[string]any{"seat": "", "reason": *reason}))
	case "grant":
		body, err := iamGrantsBody(*grants, *colleague)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return errors.New("name what to change: -grants, -colleague")
		}
		body["reason"] = *reason
		return out.written(client.patch(ctx, "/iam/people/"+subject, body))
	case "suspend", "activate":
		to := "active"
		if sub == "suspend" {
			to = "suspended"
		}
		return out.written(client.patch(ctx, "/iam/people/"+subject,
			map[string]any{"stage": to, "reason": *reason}))
	case "remove":
		return out.written(client.delete(ctx, "/iam/people/"+subject,
			withReason(*reason)))
	case "revoke":
		return out.written(client.delete(ctx,
			"/iam/people/"+subject+"/sessions", withReason(*reason)))
	case "sessions":
		return out.sessions(client.get(ctx,
			"/iam/people/"+subject+"/sessions", nil))
	case "credentials":
		return out.credentials(client.get(ctx, "/iam/credentials",
			withPerson(orSubject(*person, subject))))
	case "token":
		body := map[string]any{"label": *label}
		setValue(body, "person", orSubject(*person, subject))
		if *days > 0 {
			body["expires_in_days"] = *days
		}
		if *grants != "" {
			parsed, err := iamGrantsBody(*grants, "")
			if err != nil {
				return err
			}
			body["grants"] = parsed["grants"]
		}
		return out.token(client.post(ctx, "/iam/credentials", body))
	case "revoke-credential":
		return out.written(client.delete(ctx, "/iam/credentials/"+subject,
			withPerson(*person)))
	case "reset-mfa":
		return out.written(client.post(ctx,
			"/iam/people/"+subject+"/mfa/reset", nil))
	case "invalidate-all":
		return out.written(client.post(ctx, "/iam/invalidate-all", nil))
	case "bootstrap-code":
		return out.bootstrap(client.post(ctx, "/iam/bootstrap-code", nil))
	case "check":
		return out.check(client.get(ctx, "/iam/check", nil))
	case "audit":
		query := url.Values{}
		setIf(query, "person", orSubject(*person, subject))
		setIf(query, "event", *event)
		setIf(query, "at", *at)
		if *since > 0 {
			query.Set("since", strconv.FormatUint(*since, 10))
		}
		if *limit > 0 {
			query.Set("limit", strconv.Itoa(*limit))
		}
		return out.audit(client.get(ctx, "/iam/audit", query))
	}
	fmt.Fprintf(stdout, iamUsage, defaultBootstrapPath)
	return fmt.Errorf("unknown subcommand %q", sub)
}

// iamSubjects is what each subcommand has to be told, in one table.
//
// A TABLE RATHER THAN A CHECK PER ARM, because it runs BEFORE the config is
// read: an operator who typed `crewlet iam show` needs to be told they left
// off a person id, and a check inside the dispatch would have reported a
// missing config file first — a different problem they do not have.
//
// A subcommand with no row needs no subject, which is the safe direction: the
// far end refuses a request that names nothing, and a table that guessed
// would refuse commands that are perfectly complete.
var iamSubjects = subjectTable{
	"show":              {"a person id"},
	"invite":            {"an address"},
	"bind":              {"a person id", "a seat handle"},
	"unbind":            {"a person id"},
	"grant":             {"a person id"},
	"suspend":           {"a person id"},
	"activate":          {"a person id"},
	"remove":            {"a person id"},
	"revoke":            {"a person id"},
	"sessions":          {"a person id"},
	"reset-mfa":         {"a person id"},
	"revoke-credential": {"a credential id"},
}

// subjectTable maps a subcommand to what its positional arguments are.
type subjectTable map[string][]string

func (t subjectTable) check(sub string, given ...string) error {
	for i, what := range t[sub] {
		if i >= len(given) || strings.TrimSpace(given[i]) == "" {
			return fmt.Errorf("name %s", what)
		}
	}
	return nil
}

// orSubject prefers the flag and falls back to the positional argument, so
// `iam credentials alice` and `iam credentials -person alice` are one command.
func orSubject(flagged, positional string) string {
	if strings.TrimSpace(flagged) != "" {
		return flagged
	}
	return positional
}

func setIf(q url.Values, key, value string) {
	if strings.TrimSpace(value) != "" {
		q.Set(key, value)
	}
}

func setValue(body map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		body[key] = value
	}
}

func withReason(reason string) url.Values {
	q := url.Values{}
	setIf(q, "reason", reason)
	return q
}

func withPerson(person string) url.Values {
	q := url.Values{}
	setIf(q, "person", person)
	return q
}

// iamGrantsBody turns the two authority flags into a patch body.
//
// `none` IS A VALUE AND AN OMITTED FLAG IS NOT. Stripping somebody's last
// grant and not mentioning grants at all are opposite intentions, and an
// empty string cannot carry both — so the word is explicit, and it is the one
// spelling that could never be a grant.
func iamGrantsBody(grants, colleague string) (map[string]any, error) {
	body := map[string]any{}
	switch strings.TrimSpace(grants) {
	case "":
	case "none":
		body["grants"] = []string{}
	default:
		parts := strings.Split(grants, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		body["grants"] = out
	}
	switch level := strings.TrimSpace(colleague); level {
	case "":
	case "none", "read", "write":
		body["colleague"] = level
	default:
		return nil, fmt.Errorf("%q is not a colleague level: none, read or write",
			level)
	}
	return body, nil
}

// --- the client ---------------------------------------------------------- //

// iamClient talks to a running node's /iam surface.
type iamClient struct {
	base  string
	token string
	http  *http.Client
}

func newIAMClient(boot *config.Bootstrap, override string) (*iamClient, error) {
	base, err := nodeBaseURL(boot, override, "the company's identity directory")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken("/iam")
	if err != nil {
		return nil, err
	}
	return &iamClient{base: base, token: token,
		http: httpx.Client(apiTimeout)}, nil
}

func (c *iamClient) get(ctx context.Context, path string, query url.Values) (
	map[string]any, error) {

	return c.call(ctx, http.MethodGet, path, query, nil)
}

func (c *iamClient) post(ctx context.Context, path string, body map[string]any) (
	map[string]any, error) {

	return c.call(ctx, http.MethodPost, path, nil, body)
}

func (c *iamClient) patch(ctx context.Context, path string, body map[string]any) (
	map[string]any, error) {

	return c.call(ctx, http.MethodPatch, path, nil, body)
}

func (c *iamClient) delete(ctx context.Context, path string, query url.Values) (
	map[string]any, error) {

	return c.call(ctx, http.MethodDelete, path, query, nil)
}

// call is the one round trip, and the one place a refusal becomes a sentence.
func (c *iamClient) call(ctx context.Context, method, path string,
	query url.Values, body map[string]any) (map[string]any, error) {

	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// THE ORIGIN IS THIS NODE'S OWN. Every write on this surface is
	// origin-checked, and a CLI that sent none would be refused as a
	// cross-site request — which is the check working, on the one caller
	// it was never about.
	req.Header.Set("Origin", c.base)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, iamMaxAnswer))
	if err != nil {
		return nil, fmt.Errorf("read the answer from %s: %w", target, err)
	}
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	if resp.StatusCode >= 400 {
		return answer, iamRefusal(resp.StatusCode, answer, raw)
	}
	return answer, nil
}

// iamMaxAnswer bounds one answer this command will read.
//
// 8 MiB, which is two hundred directory rows with every field filled and an
// order of magnitude of headroom. It exists so a node answering something
// unexpected cannot make the CLI hold a company's whole log in memory.
const iamMaxAnswer = 8 << 20

// iamRefusal turns a refusal into the sentence an operator acts on.
//
// THE CODE AND THE DETAIL, because the engine's refusals carry both and the
// detail is the half that names the seat, the grant or the holder. A status
// alone would make `409` indistinguishable from `409`.
func iamRefusal(status int, answer map[string]any, raw []byte) error {
	if msg, ok := credentialRefusal(status, raw, true); ok {
		return errors.New(msg)
	}
	code, _ := answer["error"].(string)
	detail, _ := answer["detail"].(string)
	if detail == "" {
		detail, _ = answer["message"].(string)
	}
	switch {
	case code != "" && detail != "":
		return fmt.Errorf("%s: %s", code, detail)
	case code != "":
		return errors.New(code)
	case len(raw) > 0:
		return fmt.Errorf("the node answered %d: %s", status,
			strings.TrimSpace(string(raw)))
	}
	return fmt.Errorf("the node answered %d", status)
}

// --- printing ------------------------------------------------------------ //

// iamPrinter renders one answer, as a table or as the raw JSON.
type iamPrinter struct {
	w   io.Writer
	raw bool
}

func (p *iamPrinter) dump(answer map[string]any) error {
	encoded, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(p.w, string(encoded))
	return nil
}

func (p *iamPrinter) people(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["people"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLOGIN\tNAME\tSEAT\tSTAGE\tCOLLEAGUE\tGRANTS")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			str(row["id"]), dash(str(row["login"])), dash(personName(row)),
			dash(str(row["seat"])), str(row["stage"]),
			str(row["colleague"]), dash(joinAny(row["grants"])))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next := str(answer["next"]); next != "" {
		fmt.Fprintf(p.w, "\nmore: crewlet iam people -after %s\n", next)
	}
	fmt.Fprintf(p.w, "as of %s\n", str(answer["position"]))
	return nil
}

// personName is what to show where a name would go.
//
// THE THREE STATES ARE THREE SENTENCES. A removed person's key is destroyed
// and their name is gone everywhere, for ever; a sealed one is ciphertext
// this deployment's keyring cannot open, which somebody else's can; and an
// empty one is somebody who gave none. Rendering all three as blank would
// make a restore under the wrong keyring look like a company of anonymous
// people.
func personName(row map[string]any) string {
	switch {
	case truthy(row["removed"]):
		return "(removed)"
	case truthy(row["sealed"]):
		return "(sealed under a key this deployment does not have)"
	}
	return str(row["name"])
}

func (p *iamPrinter) one(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	for _, field := range []struct{ label, value string }{
		{"id", str(answer["id"])},
		{"kind", str(answer["kind"])},
		{"stage", str(answer["stage"])},
		{"login", dash(str(answer["login"]))},
		{"name", dash(personName(answer))},
		{"email", dash(str(answer["email"]))},
		{"seat", dash(str(answer["seat"]))},
		{"colleague", str(answer["colleague"])},
		{"grants", dash(joinAny(answer["grants"]))},
		{"revocation epoch", str(answer["revocation_epoch"])},
		{"version", str(answer["version"])},
	} {
		fmt.Fprintf(tw, "%s\t%s\n", field.label, field.value)
	}
	return tw.Flush()
}

func (p *iamPrinter) sessions(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["sessions"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LINEAGE\tSTARTED\tEXPIRES\tLIVE\tENDED\tREASON")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%s\n",
			str(row["lineage"]), stamp(row["created_at"]),
			stamp(row["expires_at"]), truthy(row["live"]),
			dash(stamp(row["ended_at"])), dash(str(row["ended_reason"])))
	}
	return tw.Flush()
}

func (p *iamPrinter) credentials(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["credentials"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMETHOD\tLABEL\tCREATED\tEXPIRES\tREVOKED")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\n",
			str(row["id"]), str(row["method"]), dash(str(row["label"])),
			stamp(row["created_at"]), dash(stamp(row["expires_at"])),
			truthy(row["revoked"]))
	}
	return tw.Flush()
}

func (p *iamPrinter) check(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["findings"].([]any)
	// A BINDING THE NODE COULD NOT JUDGE IS SAID FIRST, because it
	// qualifies everything below it: "nothing to report" from a node whose
	// chart applier has stalled is not a directory with no dangling
	// binding, it is one nobody could check.
	if unchecked, _ := answer["bindings_unchecked"].(float64); unchecked > 0 {
		fmt.Fprintf(p.w, "%d seat binding(s) could not be checked: this "+
			"node's org chart could not say whether their seats exist — ask "+
			"a node whose chart applier is current\n", int(unchecked))
	}
	if len(rows) == 0 {
		fmt.Fprintf(p.w, "nothing to report, as of %s\n",
			str(answer["position"]))
		return nil
	}
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FINDING\tWHO\tDETAIL")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			str(row["kind"]), dash(findingWho(row)), str(row["detail"]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(p.w, "\nas of %s\n", str(answer["position"]))
	return nil
}

// findingWho is the WHO column for one finding.
//
// A DUPLICATE NAMES EVERYBODY HOLDING THE CLAIM, prefixed by which claim — and
// by the claim's value where it is readable (a login, a seat); an address is
// named by its kind alone, because the report carries no form of it. Every
// other finding is about one person, by their login where they have one.
func findingWho(row map[string]any) string {
	if people, _ := row["people"].([]any); len(people) > 0 {
		ids := make([]string, 0, len(people))
		for _, p := range people {
			ids = append(ids, str(p))
		}
		claim := str(row["claim"])
		if value := str(row["login"]) + str(row["seat"]); value != "" {
			claim += " " + value
		}
		return claim + ": " + strings.Join(ids, ", ")
	}
	if who := str(row["login"]); who != "" {
		return who
	}
	return str(row["person"])
}

func (p *iamPrinter) audit(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	rows, _ := answer["events"].([]any)
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POSITION\tAT\tOP\tACTOR\tPERSON\tSUMMARY")
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			str(row["position"]), stamp(row["at"]), str(row["op"]),
			dash(str(row["actor"])), dash(str(row["person"])),
			dash(str(row["summary"])))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next := str(answer["next"]); next != "" && next != "0" {
		fmt.Fprintf(p.w, "\nmore: crewlet iam audit -before %s\n", next)
	}
	return nil
}

// invite prints the link ONCE, which is the only time it exists.
func (p *iamPrinter) invite(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "invitation %s\n%s\n\n", str(answer["id"]),
		str(answer["url"]))
	fmt.Fprintf(p.w, "expires %s\n", stamp(answer["expires_at"]))
	fmt.Fprintln(p.w, "This link is shown once and cannot be read back. Send "+
		"it to them yourself — this engine never sends mail.")
	return nil
}

// token prints the value ONCE, for the same reason.
func (p *iamPrinter) token(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "%s\n\n", str(answer["token"]))
	fmt.Fprintf(p.w, "credential %s, expires %s\n", str(answer["id"]),
		stamp(answer["expires_at"]))
	fmt.Fprintln(p.w, "This value is shown once. What the estate holds is a "+
		"hash of it, so nothing can read it back.")
	return nil
}

func (p *iamPrinter) bootstrap(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	fmt.Fprintf(p.w, "the one-time founder code is in %s on that node's "+
		"host, mode 0600\n", str(answer["path"]))
	fmt.Fprintln(p.w, "Every code outstanding before this one was withdrawn, "+
		"so exactly one is live.")
	return nil
}

// written prints a write's outcome, which is the three-valued answer rather
// than "done".
func (p *iamPrinter) written(answer map[string]any, err error) error {
	if err != nil {
		return err
	}
	if p.raw {
		return p.dump(answer)
	}
	if id := str(answer["id"]); id != "" {
		fmt.Fprintf(p.w, "%s\n", id)
	}
	fmt.Fprintf(p.w, "applied at %s\n", str(answer["position"]))
	if detail := str(answer["detail"]); detail != "" {
		fmt.Fprintln(p.w, detail)
	}
	return nil
}

// --- small renderings ---------------------------------------------------- //

func str(v any) string {
	switch held := v.(type) {
	case string:
		return held
	case float64:
		return strconv.FormatFloat(held, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(held)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	held, _ := v.(bool)
	return held
}

func joinAny(v any) string {
	held, _ := v.([]any)
	out := make([]string, 0, len(held))
	for _, item := range held {
		out = append(out, str(item))
	}
	return strings.Join(out, ",")
}

// stamp renders an instant in the operator's own zone.
//
// LOCAL RATHER THAN UTC, deliberately, and only here: every instant this
// engine STORES is the broker's and every comparison is between positions, so
// the one place a zone is a kindness rather than a hazard is the line a person
// reads.
func stamp(v any) string {
	raw := str(v)
	if raw == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	if parsed.IsZero() {
		return ""
	}
	return parsed.Local().Format("2006-01-02 15:04")
}
