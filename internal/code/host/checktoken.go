package host

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A CHECK'S BOX'S CREDENTIAL: a token (internal/runtoken) in the coding box's
// key domain, naming ONE attempt of one check of one commit — the row whose
// state admits the box — and the name of the repository its box was given.

// CheckUser is the Basic user name a check's box presents, its password its
// check's token: beside BoxUser, the second of the two a box route reads, and
// what tells a check's box from a coding run's before either is verified.
const CheckUser = "check"

// CheckRunRef is what a check's token admits a box to: the attempt whose row
// is the box's authority, read on every request, and the name of the
// repository the box was given.
type CheckRunRef struct {
	// Attempt is the attempt's key — the code_checks row's: its repository
	// by id, the commit checked, the check's name and the attempt's number.
	Attempt code.CheckKey
	// Repo is the repository's name the box was given — its clone URL's and
	// its API's — which a request is matched by BEFORE anything of the code
	// log is read, as a coding box's request is matched by the names its run
	// was given: a box guessing names costs the log nothing. The id beside it
	// is what makes the name the attempt's: a name its repository gave up,
	// and another repository took, never admits the box to that other.
	Repo string
}

// checkSubjectTag opens every check token's subject, and checkSubjectSep
// joins its parts.
//
// THE SAME KEY DOMAIN AS A RUN'S TOKEN, TOLD APART BY ITS SUBJECT: both are
// what a box presents on the box routes, signed under [BoxKeyDomain], and a
// run's subject opens with its launch id, a UUID, where a check's opens with
// this word, which no UUID is — so a run's token verifies as no check's and a
// check's as no run's, whatever user name presents it.
const (
	checkSubjectTag = "chk"
	checkSubjectSep = "~"
)

// checkSubjectEncoding spells the two NAMES a subject carries. A check's name
// may hold a `.` (config.ValidCodeCheckName) and so may a seat's scratch
// repository's (gitref.ScratchName), and a `.` is a token's own separator: a
// subject holding one is a token runtoken refuses and internal/redact does not
// know by its shape. Base64url, unpadded, is the alphabet runtoken's own
// signatures are written in, which holds neither a `.` nor the subject's `~`.
var checkSubjectEncoding = base64.RawURLEncoding

// CheckTokens mints and verifies a check's box's credential. Safe for
// concurrent use.
type CheckTokens struct{ signer *runtoken.Signer }

// NewCheckTokens builds them over the fleet's keyring, under BoxKeyDomain,
// refusing one that cannot sign for the fleet (runtoken.ErrNoKeyring): the
// git node whose runner mints one and the node its box dials derive one key
// from it, as a run's token's. now nil is the wall clock.
func NewCheckTokens(m runtoken.Material, now func() time.Time) (*CheckTokens, error) {
	signer, err := runtoken.New(runtoken.Options{Domain: BoxKeyDomain, Material: m, Now: now})
	if err != nil {
		return nil, err
	}
	return &CheckTokens{signer: signer}, nil
}

// Mint is a token for ref, valid for timeout — the check's own, as its runner
// holds it — and code.CheckBoxGrace past it, the box's setup: a BACKSTOP, the
// attempt's row the authority, which refuses the box the moment the attempt
// ends. Its subject is "chk~<repo id>~<commit>~<attempt>~<check>~<repository>",
// the two names base64url.
//
// A ref no attempt has — a key code.CheckKey.Validate refuses, a name no
// repository takes — or a timeout no check runs for (none, or past
// code.CheckTimeoutCeiling) is the caller's bug: a test binary panics naming
// it, and a production one answers "", which no git node admits.
func (t *CheckTokens) Mint(ref CheckRunRef, timeout time.Duration) string {
	why := invalidCheckRef(ref)
	if why == "" && (timeout <= 0 || timeout > code.CheckTimeoutCeiling) {
		why = fmt.Sprintf("a timeout of %s, outside a check's 1 s to %s (code.CheckTimeoutCeiling)", timeout,
			code.CheckTimeoutCeiling)
	}
	if why != "" {
		if testing.Testing() {
			panic("host: CheckTokens.Mint: " + why)
		}
		return ""
	}
	subject := strings.Join([]string{checkSubjectTag, ref.Attempt.RepoID, ref.Attempt.OID,
		strconv.Itoa(ref.Attempt.Attempt), checkSubjectEncoding.EncodeToString([]byte(ref.Attempt.Name)),
		checkSubjectEncoding.EncodeToString([]byte(ref.Repo))}, checkSubjectSep)
	return t.signer.Mint(subject, timeout+code.CheckBoxGrace)
}

// Verify answers the attempt a token names, false for one forged, expired,
// malformed, signed under a key this node's ring does not hold — or a RUN's
// token, whose subject opens with its launch id rather than the check's tag.
func (t *CheckTokens) Verify(token string) (CheckRunRef, bool) {
	subject := t.signer.Validate(token)
	if subject == "" {
		return CheckRunRef{}, false
	}
	parts := strings.Split(subject, checkSubjectSep)
	if len(parts) != 6 || parts[0] != checkSubjectTag {
		return CheckRunRef{}, false
	}
	attempt, err := strconv.Atoi(parts[3])
	if err != nil || strconv.Itoa(attempt) != parts[3] {
		return CheckRunRef{}, false
	}
	name, nerr := checkSubjectEncoding.DecodeString(parts[4])
	repo, rerr := checkSubjectEncoding.DecodeString(parts[5])
	if nerr != nil || rerr != nil {
		return CheckRunRef{}, false
	}
	ref := CheckRunRef{Attempt: code.CheckKey{RepoID: parts[1], OID: parts[2], Name: string(name),
		Attempt: attempt}, Repo: string(repo)}
	if invalidCheckRef(ref) != "" {
		return CheckRunRef{}, false
	}
	return ref, true
}

// invalidCheckRef is why ref names no attempt a box could be given, or "".
func invalidCheckRef(ref CheckRunRef) string {
	if err := ref.Attempt.Validate(); err != nil {
		return err.Error()
	}
	if !gitref.ValidRepoName(ref.Repo) {
		return fmt.Sprintf("repository name %q is no repository's", ref.Repo)
	}
	return ""
}

// boxCredential is the credential a box's request presented under user, as
// this fleet signed it: a coding run's token under BoxUser, a check's under
// CheckUser — the ONE RULE of which credential a user name presents, which
// the box routes' admission (admitBox) and the API's box routes on a node
// serving no code host (VerifyBox) both judge by. ok is false for any other
// user name and for a token the user's own tokens do not verify: a run's
// token under CheckUser, a check's under BoxUser.
func boxCredential(runs *BoxTokens, checks *CheckTokens, user, token string) (run BoxRunRef, check CheckRunRef,
	ok bool) {

	switch user {
	case BoxUser:
		run, ok = runs.Verify(token)
	case CheckUser:
		check, ok = checks.Verify(token)
	}
	return run, check, ok
}

// VerifyBox reports whether token is the credential user presents on a box
// route, signed by this fleet — a coding run's under BoxUser, a check's under
// CheckUser ([boxCredential]). EXPORTED FOR the API's box routes on a node
// serving no code host, which tell a box why only once its request presents
// one ([ChallengeBox]).
func VerifyBox(runs *BoxTokens, checks *CheckTokens, user, token string) bool {
	_, _, ok := boxCredential(runs, checks, user, token)
	return ok
}
