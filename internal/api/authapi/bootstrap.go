package authapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE FIRST PERSON, and the problem it solves.
//
// A fresh deployment's identity estate is EMPTY. There is nobody to invite the
// first operator, and the Tier A token that could create one is a machine
// credential rather than a person — so without this route a company's only way
// in is a token in a config file, which is the credential you least want to be
// the durable one.
//
// # The code is a FILE, not an answer
//
// It is written beside the store, 0600, by the node that mints it, and its
// SHA-256 goes on the log. Nothing ever serves it: the log line, the health
// body and the welcome screen carry its PATH, so reading it means having
// access to the host — which is the only credential a company genuinely has
// before it has any.
//
// # It closes for good
//
// The moment anybody is enrolled, the estate answers that and this route
// refuses whatever the file says. `api.auth.bootstrap: closed` refuses it from
// the start, for a deployment restored from a backup where the answer is "ask
// whoever already has an account".

// BootstrapCodeFile is the code's name beside the store.
const BootstrapCodeFile = "bootstrap-code"

// bootstrapCodeBytes is how much entropy the one-time code carries.
//
// THIRTY-TWO, which is the same floor every other credential this engine
// mints clears, and deliberately not "enough for a one-time value": the code
// creates a principal carrying the whole grant ceiling, and it sits in a file
// for as long as nobody uses it.
const bootstrapCodeBytes = 32

// bootstrapLifetime is how long a minted code stays redeemable.
//
// TWENTY-FOUR HOURS, anchored to the gesture rather than to a clock: somebody
// installs the engine and opens the dashboard, and the gap between those two
// is a working day at the outside. Longer is a credential in a file nobody
// remembers; shorter and an install started on a Friday afternoon is one
// somebody has to restart.
const bootstrapLifetime = 24 * time.Hour

// bootstrapRequest is what redeeming the code presents.
type bootstrapRequest struct {
	Code     string `json:"code"`
	Login    string `json:"login"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Bootstrap creates the first person.
//
// UNGUARDED and throttled per source, like the sign-in it stands in for: there
// is nobody to authenticate as. What bounds it is the code's own entropy, the
// throttle, and the fact that it stops existing the moment it has been used.
func (s *Service) Bootstrap(w http.ResponseWriter, r *http.Request) {
	arrived := s.now()
	source := s.sourceOf(r)
	if !s.admit(w, r, source) {
		return
	}
	open, err := s.bootstrapOpen(r)
	if err != nil {
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}
	if !open {
		// SPECIFIC, and safe to be: it says this company has started,
		// which whoever can reach an unstarted one would find out by
		// trying. What it must not do is look like a wrong code, which
		// would send somebody hunting for a file that is no longer
		// meant to work.
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeBootstrapClosed)
		return
	}

	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in bootstrapRequest
	if err := json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}

	held, err := s.readBootstrapCode()
	if err != nil {
		log.ErrorContext(r.Context(), "api_bootstrap_code_unreadable",
			"error", err, "path", s.bootstrapCodePath())
		s.refuseSignIn(w, r, arrived, source, "no code file on this node")
		return
	}
	// CONSTANT TIME, like every other credential comparison here: an early
	// exit makes the time taken depend on how much of the code was right,
	// which is a code you can guess one character at a time.
	if subtle.ConstantTimeCompare([]byte(held), []byte(in.Code)) != 1 {
		s.refuseSignIn(w, r, arrived, source, "bootstrap code mismatch")
		return
	}
	if err := credential.CheckStrength(in.Password); err != nil {
		// SPECIFIC, because the caller has already proved they hold the
		// code and the remedy is theirs to act on: a password refused
		// with the generic sign-in message would send the first
		// operator looking for a typo in a code that was right.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return
	}

	verifier, err := s.hasher.Hash(in.Password)
	if err != nil {
		log.ErrorContext(r.Context(), "api_bootstrap_hash_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	person := uuid.New().String()
	opID := "bootstrap:" + person
	if _, err := s.writer.Enrol(r.Context(), iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: in.Name, Email: in.Email, Login: in.Login,
		Credentials: []iamdomain.Credential{{
			V: iamdomain.DocumentVersion, ID: uuid.New().String(),
			Method: iamdomain.MethodPassword, Verifier: verifier,
		}},
		// THE WHOLE CEILING, and this is the ONE stated exemption in
		// the authority model: it is taken at the moment nobody holds a
		// credential, by somebody who has proved they can read a file
		// on the host, and the alternative is a first operator who
		// cannot grant themselves what they need to grant anybody
		// anything.
		Grants:    s.boot.API.Auth.MaxGrants,
		Colleague: iam.ColleagueWrite,
		OpID:      opID, Reason: "the first operator",
	}); err != nil {
		refuseEnrolment(w, r, "api_bootstrap_enrol_failed", err)
		return
	}

	// THE CODE IS SPENT ON THE LOG BEFORE THE FILE IS REMOVED, and the
	// order is the whole of it: the record is what every OTHER node reads
	// to know the company has started, and a file deleted first leaves a
	// company that has an operator and a node that cannot prove it.
	if _, err := s.writer.SpendBootstrap(r.Context(), iamdomain.BootstrapSpend{
		ID: bootstrapCodeID(held), Person: person, OpID: opID + ":spend",
		Reason: "redeemed",
	}); err != nil {
		log.WarnContext(r.Context(), "api_bootstrap_spend_failed",
			"error", err, "person", person)
	}
	// AND THE FILE GOES LAST. A failure here is logged and never reported:
	// the operator exists, the route is closed by the estate whatever the
	// file says, and telling somebody their first sign-in failed over a
	// file they cannot see would be false.
	if err := os.Remove(s.bootstrapCodePath()); err != nil {
		log.WarnContext(r.Context(), "api_bootstrap_code_not_removed",
			"error", err, "path", s.bootstrapCodePath())
	}

	log.InfoContext(r.Context(), "api_bootstrap_redeemed",
		"person", person, "login", in.Login)
	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, iamdomain.Sighting{
		ID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: in.Login, Grants: s.boot.API.Auth.MaxGrants,
		Colleague: iam.ColleagueWrite,
	})
}

// bootstrapOpen reports whether the first-person route may run at all.
//
// TWO CONDITIONS AND THE ESTATE WINS. `api.auth.bootstrap` says whether the
// route may EVER run on this deployment, and the estate says whether it still
// can — a company with people in it closes it for good whatever the file says,
// because what the route creates is an operator carrying the whole ceiling.
func (s *Service) bootstrapOpen(r *http.Request) (bool, error) {
	if s.boot.API.Auth.Bootstrap == config.BootstrapAccessClosed {
		return false, nil
	}
	held, err := s.directory.AnyPerson(r.Context())
	if err != nil {
		// THE UNKNOWN ARM, and it is reported rather than folded into
		// "closed": a caller told to ask somebody who already has an
		// account when nobody does is stuck for good, where one told to
		// try again merely waits.
		log.WarnContext(r.Context(), "api_bootstrap_estate_unreadable", "error", err)
		return false, err
	}
	return !held, nil
}

// bootstrapCodePath is where this node writes the one-time code.
//
// BESIDE THE STORE, because that is the directory an operator already has to
// be able to reach to run this engine at all — so it adds no new access
// requirement, and a code somewhere else would be one more path to explain.
func (s *Service) bootstrapCodePath() string {
	return filepath.Join(filepath.Dir(s.boot.Store.Path), BootstrapCodeFile)
}

// readBootstrapCode reads the code this node wrote.
func (s *Service) readBootstrapCode() (string, error) {
	raw, err := os.ReadFile(s.bootstrapCodePath())
	if err != nil {
		return "", err
	}
	code := string(trimSpaceBytes(raw))
	if code == "" {
		return "", fmt.Errorf("authapi: the bootstrap code file at %s is empty",
			s.bootstrapCodePath())
	}
	return code, nil
}

// WriteBootstrapCode mints a code, writes it 0600 beside the store and
// publishes its hash.
//
// CALLED AT BOOT by the node that finds an empty estate, and answering the
// PATH rather than the value: what a log line, a health body and a welcome
// screen carry is where to look, so a code never travels anywhere it could be
// read by somebody who cannot already read the host.
func (s *Service) WriteBootstrapCode(ctx context.Context, nodeID string) (string, error) {
	path := s.bootstrapCodePath()
	if _, err := os.Stat(path); err == nil {
		// ALREADY THERE, and it is not replaced: an operator may have
		// the old one open in a terminal, and minting a second would
		// silently invalidate a code somebody is about to type.
		return path, nil
	}
	raw := make([]byte, bootstrapCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authapi: mint a bootstrap code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	// 0600 AND WRITTEN BEFORE THE RECORD, so a node that crashes between
	// the two leaves a file nothing accepts rather than a record nothing
	// can satisfy.
	if err := os.WriteFile(path, []byte(code+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("authapi: write the bootstrap code to %s: %w", path, err)
	}
	if _, err := s.writer.MintBootstrap(ctx, iamdomain.BootstrapMint{
		ID: bootstrapCodeID(code), Verifier: bootstrapCodeID(code),
		MintedBy: nodeID, ExpiresAt: s.now().Add(bootstrapLifetime),
		OpID: "bootstrap-mint:" + bootstrapCodeID(code), Reason: "a fresh estate",
	}); err != nil {
		return "", fmt.Errorf("authapi: publish the bootstrap code's hash: %w", err)
	}
	return path, nil
}

// ReissueBootstrapCode withdraws every outstanding code and mints one.
//
// # Exactly one is live after it runs, which the boot path deliberately is not
//
// [Service.WriteBootstrapCode] runs at BOOT and leaves an existing file alone,
// because an operator may have the old code open in a terminal and replacing
// it silently would invalidate what they are about to type. This is the
// opposite gesture: somebody asked for a new one, so the old ones are the
// problem rather than the thing to protect — a live code is a way to become
// the first administrator with no credential at all, and an operator who
// re-issued because they lost the file has no idea the original still works.
//
// THE WITHDRAWALS GO FIRST. A crash between them and the mint leaves a
// company with NO way in, which an operator fixes by running this again; the
// other order leaves two, which nothing reports and nobody notices.
func (s *Service) ReissueBootstrapCode(ctx context.Context, nodeID string) (
	string, error) {

	outstanding, err := s.directory.OutstandingBootstrapCodes(ctx, s.now())
	if err != nil {
		return "", fmt.Errorf("authapi: read the outstanding bootstrap "+
			"codes: %w", err)
	}
	for _, code := range outstanding {
		if _, err := s.writer.WithdrawBootstrap(ctx, code.ID,
			"bootstrap-withdraw:"+code.ID,
			"superseded by a re-issued code"); err != nil {

			return "", fmt.Errorf("authapi: withdraw the bootstrap code "+
				"%s: %w", code.ID, err)
		}
	}
	path := s.bootstrapCodePath()
	// THE FILE IS REPLACED, unlike the boot path's: whoever asked for this
	// is holding the terminal, and leaving the old value in place would
	// answer a path whose contents no longer work.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("authapi: replace %s: %w", path, err)
	}
	return s.WriteBootstrapCode(ctx, nodeID)
}

// bootstrapCodeID is the SHA-256 of a code, which is what the log carries.
//
// THE HASH IS BOTH THE ID AND THE VERIFIER, deliberately: the object's
// identity IS "the code with this digest", so a separate id would be a second
// name for one thing and a redemption would have to carry both.
func bootstrapCodeID(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// trimSpaceBytes drops surrounding whitespace, so a file an operator edited
// with a trailing newline still works.
func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
