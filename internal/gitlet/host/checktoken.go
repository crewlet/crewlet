package host

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// CheckRunRef is what a check's token admits a box to: the attempt whose row,
// read on every request, is the box's authority, and the repository name the
// box was given.
type CheckRunRef struct {
	// Attempt keys the attempt's code_checks row.
	Attempt gitlet.CheckKey
	// Repo is the repository name the box was given (its clone URL's and
	// API's). A request is matched by it before the code log is read, so a box
	// guessing names costs the log nothing; Attempt.RepoID makes the name the
	// attempt's, so a name another repository later took never admits the box.
	Repo string
}

// checkSubjectTag opens every check token's subject, and checkSubjectSep
// joins its parts. A run's token shares [BoxKeyDomain], but its subject opens
// with a launch UUID, which this tag never is, so neither kind verifies as the
// other whatever user name presents it.
const (
	checkSubjectTag = "chk"
	checkSubjectSep = "~"
)

// checkSubjectEncoding spells the subject's three names (target branch, check,
// repository). Each may hold a `.` (config.ValidCodeCheckName,
// gitref.ScratchName, a ref), a token's own separator: runtoken would refuse
// the subject and internal/redact would not know it by its shape. Unpadded
// base64url is runtoken's own signature alphabet, holding neither `.` nor `~`.
var checkSubjectEncoding = base64.RawURLEncoding

// CheckTokens mints and verifies a check's box's credential. Safe for
// concurrent use.
type CheckTokens struct{ signer *runtoken.Signer }

// NewCheckTokens builds them over the fleet's keyring under BoxKeyDomain, so
// the node whose runner mints a token and the node its box dials derive one
// key, as for a run's token; it refuses a keyring that cannot sign for the
// fleet (runtoken.ErrNoKeyring). now nil is the wall clock.
func NewCheckTokens(m runtoken.Material, now func() time.Time) (*CheckTokens, error) {
	signer, err := runtoken.New(runtoken.Options{Domain: BoxKeyDomain, Material: m, Now: now})
	if err != nil {
		return nil, err
	}
	return &CheckTokens{signer: signer}, nil
}

// Mint is a token for ref valid for timeout (the check's own) plus
// gitlet.CheckBoxGrace for the box's setup: a backstop, since the attempt's
// row, which refuses the box once the attempt ends, is the authority. Its
// subject is "chk~<repo id>~<commit>~<attempt>~<target>~<check>~<repository>",
// the three names base64url.
//
// A ref no attempt has (a key gitlet.CheckKey.Validate refuses, a name no
// repository takes), or a timeout outside (0, gitlet.CheckTimeoutCeiling], is
// the caller's bug, refused with [ErrNoCheckRun], over which the runner ends
// the attempt; so is a subject runtoken refuses (runtoken.ErrSubject). A test
// binary is held to this too, for [BoxTokens.Mint]'s reason.
func (t *CheckTokens) Mint(ref CheckRunRef, timeout time.Duration) (string, error) {
	if !validCheckRef(ref) || timeout <= 0 || timeout > gitlet.CheckTimeoutCeiling {
		return "", fmt.Errorf("%w: check %q of repository %q, timeout %s", ErrNoCheckRun, ref.Attempt.Name,
			ref.Repo, timeout)
	}
	subject := strings.Join([]string{checkSubjectTag, ref.Attempt.RepoID, ref.Attempt.OID,
		strconv.Itoa(ref.Attempt.Attempt), checkSubjectEncoding.EncodeToString([]byte(ref.Attempt.Target)),
		checkSubjectEncoding.EncodeToString([]byte(ref.Attempt.Name)),
		checkSubjectEncoding.EncodeToString([]byte(ref.Repo))}, checkSubjectSep)
	token, err := t.signer.Mint(subject, timeout+gitlet.CheckBoxGrace)
	if err != nil {
		return "", fmt.Errorf("gitlet: the box credential of check %q of repository %q: %w", ref.Attempt.Name,
			ref.Repo, err)
	}
	return token, nil
}

// ErrNoCheckRun reports a ref or a timeout [CheckTokens.Mint] refuses.
var ErrNoCheckRun = errors.New("gitlet: no check's attempt runs as this, so no box credential names it")

// Verify answers the attempt a token names, false for one forged, expired,
// malformed, signed under a key this node's ring lacks, or a run's token.
func (t *CheckTokens) Verify(token string) (CheckRunRef, bool) {
	subject := t.signer.Validate(token)
	if subject == "" {
		return CheckRunRef{}, false
	}
	parts := strings.Split(subject, checkSubjectSep)
	if len(parts) != 7 || parts[0] != checkSubjectTag {
		return CheckRunRef{}, false
	}
	attempt, err := strconv.Atoi(parts[3])
	if err != nil || strconv.Itoa(attempt) != parts[3] {
		return CheckRunRef{}, false
	}
	target, terr := checkSubjectEncoding.DecodeString(parts[4])
	name, nerr := checkSubjectEncoding.DecodeString(parts[5])
	repo, rerr := checkSubjectEncoding.DecodeString(parts[6])
	if terr != nil || nerr != nil || rerr != nil {
		return CheckRunRef{}, false
	}
	ref := CheckRunRef{Attempt: gitlet.CheckKey{RepoID: parts[1], OID: parts[2], Target: string(target),
		Name: string(name), Attempt: attempt}, Repo: string(repo)}
	if !validCheckRef(ref) {
		return CheckRunRef{}, false
	}
	return ref, true
}

// validCheckRef reports whether ref names an attempt a box could be given: a
// key gitlet.CheckKey.Validate takes, and a name a repository takes.
func validCheckRef(ref CheckRunRef) bool {
	return ref.Attempt.Validate() == nil && gitref.ValidRepoName(ref.Repo)
}

// boxCredential is the credential a box's request presented under user, as
// this fleet signed it: an executor's token under BoxUser, a check's under
// gitlet.CheckUser. It is the one rule admitBox and VerifyBox both judge by;
// ok is false for any other user name and for a token the user's own kind
// does not verify (a run's under gitlet.CheckUser, a check's under BoxUser).
func boxCredential(runs *BoxTokens, checks *CheckTokens, user, token string) (run BoxRunRef, check CheckRunRef,
	ok bool) {

	switch user {
	case BoxUser:
		run, ok = runs.Verify(token)
	case gitlet.CheckUser:
		check, ok = checks.Verify(token)
	}
	return run, check, ok
}

// VerifyBox reports whether token is the credential user presents on a box
// route, signed by this fleet ([boxCredential]). Exported for the API's box
// routes on a node not serving Gitlet, which tell a box why only once its
// request presents one ([ChallengeBox]).
func VerifyBox(runs *BoxTokens, checks *CheckTokens, user, token string) bool {
	_, _, ok := boxCredential(runs, checks, user, token)
	return ok
}
