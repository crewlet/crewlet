package setupapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Test connection and the audit: each press proves a server's setup with
// certificates the engine records before it uses them, and the audit answers
// every certificate, command and verdict on a delegated request the access log
// holds, a command's text and what a verdict decided opened for the
// administrator reading it alone.

// SSHAccess is the engine's half of these routes.
type SSHAccess interface {
	// TestConnection runs the steps against in.Host, recording every test
	// certificate before it is used.
	TestConnection(ctx context.Context, in sshint.TestInput) []sshint.TestStep
	// Certificates and Commands page the access log's SSH records, and
	// Verdicts its verdicts on delegated requests.
	Certificates(ctx context.Context, q accessdomain.AuditQuery, fresh statelog.Freshness) (accessdomain.CertificatePage, error)
	Commands(ctx context.Context, q accessdomain.AuditQuery, fresh statelog.Freshness) (accessdomain.CommandPage, error)
	Verdicts(ctx context.Context, q accessdomain.VerdictQuery, fresh statelog.Freshness) (accessdomain.VerdictPage, error)
	// OpenCommand is a command's text, and OpenVerdict what a verdict decided
	// and its giver's reason and findings, for an administrator's read.
	OpenCommand(row accessdomain.CommandRow) (string, error)
	OpenVerdict(row accessdomain.VerdictRow) (subject, reason string, findings []string, err error)
}

// testRequest names which authority a press tests with.
type testRequest struct {
	CA string `json:"ca"`
}

// sshTest serves POST /setup/integrations/ssh/hosts/{id}/test: the steps of
// the server's setup proven in order, and the result kept as the server's
// latest test with that authority.
func (s *Service) sshTest(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok || !sshConfigured(w, fc.company) {
		return
	}
	block := fc.company.Integrations.SSH
	host, found := block.Host(r.PathValue("id"))
	if !found {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no SSH server " + r.PathValue("id") + " is registered"})
		return
	}
	var req testRequest
	if !readSSHBody(w, r, &req) {
		return
	}
	ref := block.Provisioning.CertificateAuthority
	switch req.CA {
	case "", "current":
	case "next":
		ref = block.Provisioning.NextCertificateAuthority
	default:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"field": "ca", "detail": "ca is current or next"})
		return
	}
	ca := s.heldAuthority(ref)
	if ca == nil {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNotConnected, map[string]string{
			"detail": "there is no " + map[bool]string{true: "staged", false: "current"}[req.CA == "next"] +
				" certificate authority the engine can sign with"})
		return
	}
	principal, how := iam.From(r.Context())
	if how != iam.Resolved || principal.Seat == "" {
		// A TEST CERTIFICATE NAMES THE PERSON who pressed it, by the seat
		// they hold, as every server's log will show it.
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden, map[string]string{
			"detail": "Test connection is pressed by a person acting as their seat, which its certificates name"})
		return
	}
	at := s.clock()
	steps := s.ssh.TestConnection(r.Context(), sshint.TestInput{
		Host: host, CA: ca, Person: principal.Seat, SourceAddresses: block.Provisioning.SourceAddresses, Now: at,
	})
	ok = len(steps) > 0
	kept := make([]coord.SSHTestStep, 0, len(steps))
	for _, step := range steps {
		ok = ok && step.OK
		kept = append(kept, coord.SSHTestStep{ID: step.ID, OK: step.OK,
			Detail: textcut.Within(step.Detail, coord.MaxSSHTestDetailBytes)})
	}
	by := auth.AttributionOf(r.Context())
	if _, err := s.sshTests.PutSSHTest(r.Context(), coord.SSHTest{
		Host: host.ID, Authority: ca.Fingerprint(), HostDigest: sshint.HostDigest(host), At: at,
		By: by.Name, ByKind: by.Kind,
		OperatorID: by.OperatorID, OK: ok, Steps: kept,
	}); err != nil {
		log.WarnContext(r.Context(), "setup_ssh_test_unrecorded", "host", host.ID, "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"reason": "the test ran, and its result could not be kept; press it again",
				"steps": steps})
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"host": host.ID, "authority": ca.Fingerprint(), "ok": ok, "steps": steps, "at": at})
}

// verdictFilters are the audit's filters only a verdict carries, refused on
// another kind rather than ignored, so a page never answers wider than asked.
var verdictFilters = []string{"verdict", "by_kind", "delegation_kind", "by", "request", "final", "sampled"}

// sshAudit serves GET /setup/integrations/ssh/audit: certificates, commands or
// verdicts on delegated requests, newest first, narrowed by seat, host and
// time; a command's text, and what a verdict decided with its giver's words,
// opened.
func (s *Service) sshAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := accessdomain.AuditQuery{Handle: q.Get("seat"), Host: q.Get("host"), Cursor: q.Get("cursor")}
	for name, into := range map[string]*time.Time{"since": &query.Since, "until": &query.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
					"field": name, "detail": name + " is an RFC 3339 instant, such as 2026-11-08T09:00:00Z"})
				return
			}
			*into = t
		}
	}
	if v := q.Get("limit"); v != "" {
		// THE READER'S OWN BOUND, never a number of the route's: a limit the
		// route admitted and the reader refused was answered 400 anyway.
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > accessdomain.MaxPageSize {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
				"field": "limit", "detail": "limit is 1 to " + strconv.Itoa(accessdomain.MaxPageSize)})
			return
		}
		query.Limit = n
	}
	fresh := statelog.Freshness{Level: statelog.ReadConsistentPrefix}
	kind := strings.TrimSpace(q.Get("kind"))
	if kind != "verdict" {
		for _, name := range verdictFilters {
			if q.Has(name) {
				httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
					"field": name, "detail": name + " narrows kind=verdict alone"})
				return
			}
		}
	}
	switch kind {
	case "", "certificate":
		page, err := s.ssh.Certificates(r.Context(), query, fresh)
		if s.auditFailed(w, r, err) {
			return
		}
		rows := make([]map[string]any, 0, len(page.Rows))
		for _, c := range page.Rows {
			rows = append(rows, map[string]any{"seat": c.Handle, "serial": c.Serial, "key_id": c.KeyID,
				"turn": c.Turn, "held_call": c.HeldCall, "purpose": c.Purpose, "host_ids": c.HostIDs,
				"principals": c.Principals, "valid_after": c.ValidAfter, "valid_before": c.ValidBefore,
				"public_key_sha256": c.PublicKeySHA256, "ca_sha256": c.CASHA256, "node": c.Node, "at": c.At})
		}
		httpjson.Write(w, http.StatusOK, map[string]any{"kind": "certificate", "rows": rows,
			"next": page.Next, "complete": page.Complete})
	case "command":
		page, err := s.ssh.Commands(r.Context(), query, fresh)
		if s.auditFailed(w, r, err) {
			return
		}
		rows := make([]map[string]any, 0, len(page.Rows))
		for _, c := range page.Rows {
			text, openErr := s.ssh.OpenCommand(c)
			row := map[string]any{"seat": c.Handle, "id": c.ID, "turn": c.Turn, "held_call": c.HeldCall,
				"host": c.Host, "user": c.User, "level": c.Level, "stdin_bytes": c.StdinBytes,
				"stdin_sha256": c.StdinSHA256, "ran": c.Ran, "exit_code": c.ExitCode,
				"duration_ms": c.Duration.Milliseconds(), "stdout_bytes": c.StdoutBytes,
				"stderr_bytes": c.StderrBytes, "output_sha256": c.OutputSHA256, "truncated": c.Truncated,
				"approver": c.Approver, "decision": c.Decision, "certificate_serial": c.CertificateSerial,
				"failure": c.Failure, "at": c.At}
			if openErr == nil {
				row["command"] = text
			} else {
				row["command_unopened"] = "the command's text did not open under this node's keyring"
			}
			rows = append(rows, row)
		}
		httpjson.Write(w, http.StatusOK, map[string]any{"kind": "command", "rows": rows,
			"next": page.Next, "complete": page.Complete})
	case "verdict":
		vq, ok := verdictQuery(w, q, query)
		if !ok {
			return
		}
		page, err := s.ssh.Verdicts(r.Context(), vq, fresh)
		if s.auditFailed(w, r, err) {
			return
		}
		rows := make([]map[string]any, 0, len(page.Rows))
		for _, v := range page.Rows {
			row := map[string]any{"seat": v.Handle, "request": v.Request, "kind": v.Kind, "rule": v.Rule,
				"turn": v.Turn, "by": v.By, "by_kind": v.ByKind, "through": v.Through, "asked": v.Asked,
				"confidence": v.Confidence, "verdict": v.Verdict, "final": v.Final,
				"overruled": nonNilWords(v.Overruled), "sampled": v.Sampled, "digest": v.Digest,
				"host": v.Host, "user": v.User, "stdin_bytes": v.StdinBytes, "stdin_sha256": v.StdinSHA256,
				"decision": v.Decision, "item": v.Item, "at": v.At}
			if subject, reason, findings, openErr := s.ssh.OpenVerdict(v); openErr == nil {
				row["subject"], row["reason"], row["findings"] = subject, reason, nonNilWords(findings)
			} else {
				row["subject_unopened"] = "the verdict's texts did not open under this node's keyring"
			}
			rows = append(rows, row)
		}
		httpjson.Write(w, http.StatusOK, map[string]any{"kind": "verdict", "rows": rows,
			"next": page.Next, "complete": page.Complete})
	default:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"field": "kind", "detail": "kind is certificate, command or verdict"})
	}
}

// verdictQuery is the audit's query with the verdict filters q names, or false
// once a malformed one is answered. Each word is held to its closed set by the
// reader, which names it.
func verdictQuery(w http.ResponseWriter, q url.Values, audit accessdomain.AuditQuery) (
	accessdomain.VerdictQuery, bool) {

	out := accessdomain.VerdictQuery{
		Handle: audit.Handle, Host: audit.Host, By: q.Get("by"), Request: q.Get("request"),
		Kind: delegated.Kind(q.Get("delegation_kind")), Verdict: delegated.Verdict(q.Get("verdict")),
		ByKind: accessdomain.VerdictBy(q.Get("by_kind")),
		Since:  audit.Since, Until: audit.Until, Cursor: audit.Cursor, Limit: audit.Limit,
	}
	for name, into := range map[string]**bool{"final": &out.Final, "sampled": &out.Sampled} {
		switch q.Get(name) {
		case "":
		case "true":
			*into = new(true)
		case "false":
			*into = new(false)
		default:
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
				"field": name, "detail": name + " is true or false"})
			return accessdomain.VerdictQuery{}, false
		}
	}
	return out, true
}

// nonNilWords is a list that encodes as an array, never as null.
func nonNilWords(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// auditFailed answers an audit read that failed, reporting whether it did.
func (s *Service) auditFailed(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, accessdomain.ErrInvalid):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"detail": strings.TrimPrefix(err.Error(), "accessdomain: ")})
	default:
		log.WarnContext(r.Context(), "setup_ssh_audit_unread", "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"reason": "the access log could not be read; try again"})
	}
	return true
}
