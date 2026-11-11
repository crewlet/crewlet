package setupapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Test connection and the audit: each press proves a server's setup with
// certificates the engine records before it uses them, and the audit answers
// every certificate and command the access log holds, a command's text opened
// for the administrator reading it alone.

// SSHAccess is the engine's half of these routes.
type SSHAccess interface {
	// TestConnection runs the steps against in.Host, recording every test
	// certificate before it is used.
	TestConnection(ctx context.Context, in sshint.TestInput) []sshint.TestStep
	// Certificates and Commands page the access log's SSH records.
	Certificates(ctx context.Context, q accessdomain.AuditQuery, fresh statelog.Freshness) (accessdomain.CertificatePage, error)
	Commands(ctx context.Context, q accessdomain.AuditQuery, fresh statelog.Freshness) (accessdomain.CommandPage, error)
	// OpenCommand is a command's text, for an administrator's read.
	OpenCommand(row accessdomain.CommandRow) (string, error)
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

// sshAudit serves GET /setup/integrations/ssh/audit: certificates or commands,
// newest first, narrowed by seat, host and time; a command's text opened.
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
		n, err := strconv.Atoi(v)
		// THE READER'S OWN BOUND, never a second number: a page past it is one
		// the reader refuses, answered as the request's fault.
		if err != nil || n < 1 || n > accessdomain.MaxPageSize {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
				"field": "limit", "detail": "limit is 1 to " + strconv.Itoa(accessdomain.MaxPageSize)})
			return
		}
		query.Limit = n
	}
	fresh := statelog.Freshness{Level: statelog.ReadConsistentPrefix}
	switch kind := strings.TrimSpace(q.Get("kind")); kind {
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
	default:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalid, map[string]string{
			"field": "kind", "detail": "kind is certificate or command"})
	}
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
