package authapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
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
// SHA-256 goes on the log. Nothing ever serves it: the log line and the
// re-issue's answer carry its PATH, so reading it means having access to the
// host — which is the only credential a company genuinely has before it has
// any.
//
// # The LOG is the check, so any node redeems any node's code
//
// A presented code is hashed and looked up on the identity log, and what the
// log says about it is the whole answer: live, redeemed, aged out, withdrawn,
// or nothing. The file is not consulted to ACCEPT anything — a fleet behind a
// load balancer puts the founder's request on whichever node it likes, and the
// route used to compare against the serving node's own file, so every node but
// the one that wrote the code refused it as a wrong one. The file is read only
// to recognise a code this node wrote whose mint never reached the log, which
// is a code its holder can replace rather than one they typed wrong.
//
// # It lives twenty-four hours, and one command replaces it
//
// A code that aged out, was withdrawn by a re-issue or never reached the log
// answers `410 bootstrap_code_stale` naming the remedy — `crewlet iam
// bootstrap-code`, or a restart of the node that wrote it — and never the
// closed answer, which is permanent and would send a founder away from a
// company still waiting for them. It is specific without being an oracle: the
// arm is reachable only by presenting a code whose digest is on the log or in
// this node's own file, which a stranger cannot do.
//
// # One founding at a time, and the one that stopped is finished or ended
//
// A founding TAKES the first-person exemption on the company's one bootstrap
// subject before it claims anything ([iamdomain.Writer.Enrol]), so a second
// code presented while another founder's enrolment is in progress answers
// `409 bootstrap_in_progress` naming when that one lapses — and a founder whose
// own attempt stopped halfway finishes it with the same code, or, once that
// code has died, with a fresh one: the fresh founding ends the stopped attempt
// before it claims the address that attempt was holding.
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
// for as long as nobody uses it. It is also what makes the code's digest a
// safe thing to look up by: nobody finds a preimage of a row on the log.
const bootstrapCodeBytes = 32

// bootstrapLifetime is how long a minted code stays redeemable.
//
// TWENTY-FOUR HOURS, anchored to the gesture rather than to a clock: somebody
// installs the engine and opens the dashboard, and the gap between those two
// is a working day at the outside. Longer is a credential in a file nobody
// remembers; shorter and an install started on a Friday afternoon is one
// somebody has to restart. What makes the number safe to be short is that
// running out costs one command: a restart replaces a dead file on the node
// that holds it ([Service.OfferBootstrapCode]), and `crewlet iam
// bootstrap-code` mints a fresh one on whichever node serves it.
const bootstrapLifetime = 24 * time.Hour

// bootstrapRequest is what redeeming the code presents.
type bootstrapRequest struct {
	Code string `json:"code"`

	// Login is REQUIRED, in the person grammar (jane.doe): the first
	// operator is a person like every other, and a person with no login
	// is recorded as nobody beside every change they make. An absent one
	// is refused 400 by the enrolment, naming the rule.
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
	if !s.admit(w, r, source, types.FailBootstrap) {
		return
	}
	closed, err := s.bootstrapClosed(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_bootstrap_estate_unreadable", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	if closed != "" {
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

	// SURROUNDING WHITESPACE IS NOT PART OF A CODE — the alphabet is
	// base64url — and the file's own read trims it, so a code pasted with
	// the newline it was copied with is the code in the file.
	presented := strings.TrimSpace(in.Code)
	code, live := s.liveCode(w, r, arrived, source, presented)
	if !live {
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
	// THE FIRST OPERATOR IS THE CODE'S, DERIVED rather than minted — see
	// [iamdomain.BootstrapCode.FounderID]. Minted per request, a bootstrap
	// that stopped after its address claim left that address held for an
	// id no retry named, and the founder's corrected retry was refused as
	// "that address belongs to somebody" by their own first attempt.
	person := code.FounderID()
	opID := "bootstrap:" + code.ID
	enrolled, err := s.writer.Enrol(r.Context(), iamdomain.Enrolment{
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
		//
		// THE CODE IS THE AUTHORITY, named on the enrolment: the domain
		// TAKES it on the company's one bootstrap subject before the
		// first claim, and checks the take again in the snapshot the
		// grants land from — this attempt's, current, and nobody else
		// enrolled. The node's own writer could not confer the ceiling on
		// its own grants, and must not be able to.
		Grants:        s.boot.API.Auth.MaxGrants,
		Colleague:     iam.ColleagueWrite,
		BootstrapCode: code.ID,
		OpID:          opID, Reason: "the first operator",
	})
	if err != nil {
		s.refuseFounder(w, r, arrived, source, presented, err)
		return
	}
	if !landed(enrolled) {
		// NOTHING IS BUILT ON AN ENROLMENT NOBODY CAN CONFIRM: no file
		// removed, no session. The op id is the code's own, so presenting
		// the same code again is the same enrolment — and the code is
		// TAKEN by it, so it is the one code that may finish it.
		unresolved(w, r, "api_bootstrap_enrol_unresolved", enrolled)
		return
	}

	// THIS NODE'S FILE GOES LAST, whichever code it holds, and only once
	// the person is on the log: the enrolment is what every OTHER node
	// reads to know the company has started, and a file deleted first
	// would leave a company with nobody in it and a node with no code to
	// offer. The company has started, so no code will ever be honoured
	// again. A failure here is
	// logged and never reported — the operator exists, the route is closed
	// by the estate whatever the file says, and telling somebody their
	// first sign-in failed over a file they cannot see would be false. A
	// file on ANOTHER node goes at that node's next boot
	// ([Service.OfferBootstrapCode]).
	s.codeMu.Lock()
	s.removeCodeFile(r.Context(), "the company's first person was created")
	s.codeMu.Unlock()

	log.InfoContext(r.Context(), "api_bootstrap_redeemed",
		"person", person, "login", in.Login)
	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, iamdomain.Sighting{
		ID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: in.Login, Grants: s.boot.API.Auth.MaxGrants,
		Colleague: iam.ColleagueWrite,
	}, signIn{method: types.SignInBootstrap})
}

// liveCode is the code a caller presented, when it is one a founding may go
// on with, answering false once it has written the refusal.
//
// THE LOG DECIDES, by [iamdomain.BootstrapCode.State] — the predicate the
// founding's own decides ask — so this route and the record cannot disagree
// about a code. Each answer is a different remedy:
//
//   - LIVE is the way in.
//   - TAKEN is a founding begun with this very code that has not finished —
//     a request that stopped after its take — and presenting the code again
//     is what finishes it, so it goes on exactly as a live one does.
//   - REDEEMED is a company that has started on a node that has not applied
//     its first person yet: the closed answer, which is permanent.
//   - AGED OUT and WITHDRAWN are a real code that no longer works, and so is
//     a code this node's OWN FILE holds whose mint never reached the log:
//     `410 bootstrap_code_stale`, whose remedy is one command.
//   - NOTHING — no row, and not this node's file — is a wrong code, and is
//     refused exactly as every failed sign-in is.
func (s *Service) liveCode(w http.ResponseWriter, r *http.Request,
	arrived time.Time, source, presented string) (iamdomain.BootstrapCode, bool) {

	code, err := s.directory.BootstrapCode(r.Context(), bootstrapCodeID(presented))
	if err != nil {
		log.WarnContext(r.Context(), "api_bootstrap_codes_unreadable", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return iamdomain.BootstrapCode{}, false
	}
	switch state := code.State(s.now()); state {
	case iamdomain.CodeLive, iamdomain.CodeTaken:
		return code, true
	case iamdomain.CodeRedeemed:
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeBootstrapClosed)
	case iamdomain.CodeAgedOut, iamdomain.CodeWithdrawn:
		s.refuseStaleCode(w, r, arrived, source, presented, string(state))
	default:
		// THE FILE IS READ ONLY HERE, and only to tell two absences
		// apart: a code this node wrote whose mint never landed is one
		// its holder replaces, and anything else is a code they typed
		// wrong. CONSTANT TIME, like every credential comparison here:
		// an early exit makes the time taken depend on how much of the
		// code was right, which is a file read one character at a time.
		if held, err := s.readBootstrapCode(); err == nil &&
			subtle.ConstantTimeCompare([]byte(held), []byte(presented)) == 1 {
			s.refuseStaleCode(w, r, arrived, source, presented, "unpublished")
			return iamdomain.BootstrapCode{}, false
		}
		// THE CODE TRIED IS THE SUBJECT, keyed in memory and never
		// kept: how many DIFFERENT codes one client tried in a minute is
		// the difference between a typo and somebody guessing.
		s.refuseSignIn(w, r, arrived, authevents.Failure{
			Client: source, Method: types.FailBootstrap, Subject: presented,
		}, "bootstrap code matches nothing on the log")
	}
	return iamdomain.BootstrapCode{}, false
}

// refuseStaleCode answers a REAL code that no longer works: aged out,
// withdrawn by a re-issue, or written by this node and never published.
//
// # 410, its own code, and a remedy — never the closed answer
//
// A founder installing on a Friday and opening the dashboard on Monday holds a
// code a day past its lifetime. Told `bootstrap_closed` they read "this company
// has started" and give up on a company that is still waiting for them; told
// `sign_in_refused` they go looking for a typo in a code that was right. What
// is true is that the file is stale and one command replaces it, so that is
// what the answer says.
//
// # A failed attempt like every other
//
// Counted against the SOURCE, reaching the failure tally and padded to the
// deadline, as an invitation's 410 is — a refusal is a refusal whichever arm
// produced it, and the specificity is safe because presenting a code whose
// digest is on the log, or in this node's own file, is proof of holding one.
func (s *Service) refuseStaleCode(w http.ResponseWriter, r *http.Request,
	arrived time.Time, source, presented, state string) {

	s.throttle.Fail(r.Context(), source)
	s.audit.Failed(r.Context(), authevents.Failure{
		Client: source, Method: types.FailBootstrap, Subject: presented,
	})
	log.WarnContext(r.Context(), "api_bootstrap_code_stale",
		"state", state, "source", source,
		"hint", "`crewlet iam bootstrap-code` mints a fresh one, and a "+
			"restart replaces a dead file on the node that holds it")
	s.throttle.Pad(r.Context(), arrived)
	httpjson.Fail(w, http.StatusGone, httpjson.CodeBootstrapCodeStale)
}

// refuseFounder answers an enrolment the record refused.
//
// THE RECORD'S SENTINEL PICKS THE ANSWER, because the founding read the code
// and the directory in its own snapshots, which are later than anything this
// route read: a code that died in between is the stale answer, somebody else
// becoming the first person is the closed one, another founder's enrolment in
// progress is its own, and any other refusal of the node's own writer is a
// wiring fault no caller can clear.
func (s *Service) refuseFounder(w http.ResponseWriter, r *http.Request,
	arrived time.Time, source, presented string, err error) {

	var inProgress *iamdomain.FoundingInProgress
	switch {
	case errors.As(err, &inProgress):
		// NOT COUNTED as a failed attempt, for the closed answer's reason:
		// it is a state of the company rather than a wrong credential, and
		// it is reached only by presenting a live code. What it says is
		// the one thing the holder can act on without a command — when
		// their code may be taken.
		log.InfoContext(r.Context(), "api_bootstrap_in_progress",
			"until", inProgress.Until)
		httpjson.FailWith(w, http.StatusConflict,
			httpjson.CodeBootstrapInProgress, map[string]string{
				"until": inProgress.Until.UTC().Format(time.RFC3339),
			})
	case errors.Is(err, iamdomain.ErrBootstrapCodeDead):
		s.refuseStaleCode(w, r, arrived, source, presented, "dead at the record")
	case errors.Is(err, iamdomain.ErrBootstrapClosed):
		log.InfoContext(r.Context(), "api_bootstrap_closed_at_the_record",
			"error", err)
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeBootstrapClosed)
	case errors.Is(err, iamdomain.ErrRefused):
		// THE NODE'S OWN WRITER REFUSED, which is its grants or a
		// ceiling this build cannot name — a deployment fault, not the
		// founder's, and not one waiting clears.
		log.ErrorContext(r.Context(), "api_bootstrap_writer_refused",
			"error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	default:
		refuseEnrolment(w, r, "api_bootstrap_enrol_failed", err)
	}
}

// bootstrapClosed says why the first-person route may not run, or "" while it
// may.
//
// TWO CONDITIONS AND THE ESTATE WINS. `api.auth.bootstrap` says whether the
// route may EVER run on this deployment, and the estate says whether it still
// can — a company with people in it closes it for good whatever the file says,
// because what the route creates is an operator carrying the whole ceiling.
//
// ONE GATE FOR EVERY PARTY THAT ASKS: the route, the posture read's open flag,
// the boot path deciding whether to offer a code and the re-issue. The estate
// half is [iamdomain.Reader.AnyPerson], which is the predicate the record and
// the mint are refused on, so a reservation left by a refused attempt is
// nobody here too.
//
// THE UNKNOWN ARM IS AN ERROR rather than folded into "closed": a caller told
// to ask somebody who already has an account when nobody does is stuck for
// good, where one told to try again merely waits.
func (s *Service) bootstrapClosed(ctx context.Context) (string, error) {
	if s.boot.API.Auth.Bootstrap == config.BootstrapAccessClosed {
		return "api.auth.bootstrap is closed on this deployment", nil
	}
	held, err := s.directory.AnyPerson(ctx)
	if err != nil {
		return "", err
	}
	if held {
		return "somebody is already enrolled in this company", nil
	}
	return "", nil
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

// OfferBootstrapCode makes sure this node's code file holds a code the log
// honours, and answers its path — or "" where this node offers none.
//
// CALLED AT BOOT, and answering the PATH rather than the value: what a log line
// carries is where to look, so a code never travels anywhere it could be read
// by somebody who cannot already read the host.
//
// # A code that still works is kept; anything else is replaced
//
// A code the log holds as LIVE is not replaced — an operator may have it open
// in a terminal, and minting another would silently invalidate a code somebody
// is about to type — and nor is one a founding TOOK and has not finished:
// that code is the only one that finishes the founder's own stopped attempt
// before it lapses. Everything else is replaced by a fresh one: a code that
// aged out, one a re-issue withdrew, one whose mint never reached the log, an
// empty or unreadable file. The boot path used to keep ANY file, so a node
// restarted a day after its first boot advertised a code the log had already
// aged out, and the founder who read it was refused as having typed it wrong.
//
// # Only this node's file, and nobody else's code
//
// The node that holds a file is the only one that can see it, so each node
// replaces its OWN at its own boot, and a code on another host is that host's
// to replace. Nothing live is withdrawn here, whoever minted it: a fleet
// booting on an empty estate offers one code per node, and a founder may be
// typing any of them. `crewlet iam bootstrap-code` is the gesture that ends
// every other code ([Service.ReissueBootstrapCode]).
//
// # A company that has started keeps no file
//
// Once anybody is enrolled — or `api.auth.bootstrap` is closed — no code will
// ever be honoured, so a file left from before is removed rather than left on
// the host as a superuser claim that merely happens not to work. This is what
// clears the file on every node but the one that served the redemption.
func (s *Service) OfferBootstrapCode(ctx context.Context, nodeID string) (string, error) {
	s.codeMu.Lock()
	defer s.codeMu.Unlock()

	closed, err := s.bootstrapClosed(ctx)
	if err != nil {
		return "", fmt.Errorf("authapi: read whether this company has "+
			"started: %w", err)
	}
	if closed != "" {
		s.removeCodeFile(ctx, closed)
		return "", nil
	}
	path := s.bootstrapCodePath()
	held, err := s.readBootstrapCode()
	switch {
	case err == nil:
		code, err := s.directory.BootstrapCode(ctx, bootstrapCodeID(held))
		if err != nil {
			return "", fmt.Errorf("authapi: read whether the code in %s is "+
				"live: %w", path, err)
		}
		state := code.State(s.now())
		if state == iamdomain.CodeLive || state == iamdomain.CodeTaken {
			return path, nil
		}
		log.WarnContext(ctx, "api_bootstrap_code_replaced", "path", path,
			"state", string(state),
			"detail", "the code in this file is not one the log honours, "+
				"so a fresh one replaces it")
	case errors.Is(err, fs.ErrNotExist):
	default:
		log.WarnContext(ctx, "api_bootstrap_code_replaced", "path", path,
			"error", err)
	}
	return s.mintCodeFile(ctx, nodeID, s.writer.MintBootstrap)
}

// ReissueBootstrapCode withdraws every outstanding code, ends an unfinished
// founding, and mints one — so that where its mint lands, exactly one code is
// live and no founding is in progress.
//
// # Exactly one is live after it runs, which the boot path deliberately is not
//
// [Service.OfferBootstrapCode] keeps a code that still works, because an
// operator may have it open in a terminal and replacing it silently would
// invalidate what they are about to type. This is the opposite gesture:
// somebody asked for a new one, so the old ones are the problem rather than
// the thing to protect — a live code is a way to become the first
// administrator with no credential at all, and an operator who re-issued
// because they lost the file has no idea the original still works. The file
// lands on THIS node's host, and the caller is told which node that is.
//
// ONE GESTURE IN THE DOMAIN, decided where the mint lands
// ([iamdomain.Writer.ReissueBootstrap]). It used to read the outstanding codes
// here, withdraw them one by one and then mint, so a code another node minted
// between the read and the mint — a boot, or a second operator re-issuing — was
// live beside the new one, and "exactly one" was two.
//
// REFUSED, wrapping [iamdomain.ErrBootstrapClosed], once the route is closed —
// by the same gate the route asks, so a re-issue can never mint a code the
// route would refuse. It used to ask whether an active, credentialled
// administrator existed instead, and handed a company whose only person was
// suspended a code the record then refused.
func (s *Service) ReissueBootstrapCode(ctx context.Context, nodeID string) (
	string, error) {

	s.codeMu.Lock()
	defer s.codeMu.Unlock()

	closed, err := s.bootstrapClosed(ctx)
	if err != nil {
		return "", fmt.Errorf("authapi: read whether this company has "+
			"started: %w", err)
	}
	if closed != "" {
		return "", fmt.Errorf("%w: %s", iamdomain.ErrBootstrapClosed, closed)
	}
	return s.mintCodeFile(ctx, nodeID, s.writer.ReissueBootstrap)
}

// mintCodeFile writes a fresh code over this node's file and publishes its
// hash with publish — a boot's mint, or a re-issue. The caller holds codeMu.
//
// # The file is REPLACED, never edited
//
// Written to a temporary file in the same directory and renamed over the old
// one, so a crash mid-write never leaves half a code, and the file is 0600
// whatever the old one's mode was — a write into an existing file keeps that
// file's permissions, which is how a code could land in a world-readable file
// somebody had created by hand.
//
// # Written before the record, and removed if the record does not land
//
// A node that crashes between the two leaves a file nothing accepts rather
// than a record nothing can satisfy, and the next boot replaces it. A mint the
// log REFUSED — a company that started a moment ago — or that failed outright
// takes its file with it, and one whose outcome is UNKNOWN keeps it without
// offering it, so a file on the host is only ever a code the log honours or
// one a restart replaces.
func (s *Service) mintCodeFile(ctx context.Context, nodeID string,
	publish func(context.Context, iamdomain.BootstrapMint) (statelog.Result, error)) (
	string, error) {

	path := s.bootstrapCodePath()
	raw := make([]byte, bootstrapCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authapi: mint a bootstrap code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	if err := replaceFile(path, []byte(code+"\n")); err != nil {
		return "", fmt.Errorf("authapi: write the bootstrap code to %s: %w",
			path, err)
	}
	id := bootstrapCodeID(code)
	minted, err := publish(ctx, iamdomain.BootstrapMint{
		ID: id, Verifier: id, MintedBy: nodeID,
		ExpiresAt: s.now().Add(bootstrapLifetime),
		OpID:      "bootstrap-mint:" + id, Reason: "a fresh estate",
	})
	if err != nil {
		s.removeCodeFile(ctx, "its mint did not land")
		return "", fmt.Errorf("authapi: publish the bootstrap code's hash: %w", err)
	}
	if !landed(minted) {
		// THE FILE STAYS on a mint nobody can confirm, and the path is not
		// offered. Removing it would be wrong in the case that matters: a
		// mint that DID land is a live code, and a file gone from under it
		// sends the next boot to mint a second one beside it. Kept, it is
		// either that live code or one the next boot finds unhonoured and
		// replaces — which is the whole of what a file here may be.
		return "", fmt.Errorf("authapi: the bootstrap code's hash (operation "+
			"%s) has an unknown outcome; retry: %w", minted.OpID,
			ErrUnresolved)
	}
	return path, nil
}

// removeCodeFile removes this node's code file, if there is one. The caller
// holds codeMu.
//
// LOGGED AND NEVER RETURNED: every caller has already decided the file must go,
// and a file that could not be removed is a code the log will not honour —
// the next boot tries again.
func (s *Service) removeCodeFile(ctx context.Context, why string) {
	path := s.bootstrapCodePath()
	err := os.Remove(path)
	switch {
	case err == nil:
		log.InfoContext(ctx, "api_bootstrap_code_removed", "path", path,
			"reason", why)
	case !errors.Is(err, fs.ErrNotExist):
		log.WarnContext(ctx, "api_bootstrap_code_not_removed",
			"error", err, "path", path, "reason", why)
	}
}

// replaceFile writes data to path through a temporary file and a rename, so the
// file is either the old one or the whole new one, and is 0600.
func replaceFile(path string, data []byte) error {
	// os.CreateTemp creates the file 0600, which is the mode the code
	// needs — and a rename carries the temporary file's mode over, never
	// the replaced one's.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
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
