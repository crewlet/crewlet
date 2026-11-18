package host

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// A CODING BOX'S GIT CREDENTIAL: a per-run token (internal/runtoken) in a key
// domain of its own, naming the run's turn and its current launch.

// BoxKeyDomain separates a coding box's git credential from every other
// per-run token: an OTLP or bridge token presented on /box/git/ is a
// signature under another key, and refused.
const BoxKeyDomain = "crewlet.git.v1"

// BoxUser is the Basic user name a box presents, its password the token.
const BoxUser = "run"

// RunTokenTTL is how long a box's credential is valid: a BACKSTOP, never the
// revocation. The run's ROW is the authority, read on every request ([Runs]):
// a run that ended, was reaped or launched again is refused once its row says
// so. An executor has no wall-clock limit (sandbox.Limits), so a short expiry
// would fail a long job's only durable output, its last push. Seven days
// (config.DefaultMinAge, the trim's replay window) bounds the one case the
// row cannot, a live row whose box is gone, which the completion poll or the
// seat's next holder settles sooner; a copied token is worthless past either.
// It floors a keyring rotation's last step, as an old key dropped sooner
// refuses running boxes' pushes (docs/concepts/secret-store.md), and is a
// ceiling: with sandbox.KeptBoxMaxAge it may not pass gitlet.PruneGrace, held
// at compile time in land.go ([Server.forged]).
const RunTokenTTL = 7 * 24 * time.Hour

// BoxRunRef names a run's ONE job: the row's key (the turn's run id) and the
// launch the token was minted for. The launch id, which the store mints at
// every launch, is what a superseded job's token stops matching; a token
// naming the turn alone would keep admitting the first box after a relaunch.
type BoxRunRef struct {
	TurnID   string
	LaunchID string
}

// maxBoxTurnBytes bounds a token's turn id: every turn id is a UUIDv7 (36
// bytes), so 128 is past any the engine mints and keeps the subject, which
// git sends in every request's header, short.
const maxBoxTurnBytes = 128

// boxSubjectSep joins a token's launch id to its turn id: the LAUNCH FIRST, a
// UUID that holds none, so the parse is a cut at the first separator whatever
// the turn id holds — mcpbridge's separator, for its reasons.
const boxSubjectSep = "~"

// BoxTokens mints and verifies box credentials. Safe for concurrent use.
type BoxTokens struct{ signer *runtoken.Signer }

// NewBoxTokens builds them over the fleet's keyring under BoxKeyDomain,
// refusing one that cannot sign for the fleet (runtoken.ErrNoKeyring): the
// node that launched a run and the Gitlet node its box dials derive one key,
// and a rotation in flight verifies by every key the ring still holds. now
// nil is the wall clock.
func NewBoxTokens(m runtoken.Material, now func() time.Time) (*BoxTokens, error) {
	signer, err := runtoken.New(runtoken.Options{Domain: BoxKeyDomain, Material: m, Now: now})
	if err != nil {
		return nil, err
	}
	return &BoxTokens{signer: signer}, nil
}

// Mint is a token for ref, valid RunTokenTTL. Its subject is
// "<launch id>~<turn id>".
//
// A ref no run has (every launch id is a UUID the store mints, every turn id
// a UUIDv7) is the caller's bug, refused with [ErrNoBoxRun], which every
// caller refuses its launch over, rather than minted into a token another
// run's parse could read; so is a subject runtoken refuses
// (runtoken.ErrSubject). Never a panic in tests alone: telling one apart
// takes the `testing` package, which the binary must not link (cmd/crewlet's
// TestTheBinaryLinksNoTestScaffolding).
func (t *BoxTokens) Mint(ref BoxRunRef) (string, error) {
	if !validRef(ref) {
		return "", fmt.Errorf("%w: launch %q of run %q", ErrNoBoxRun, ref.LaunchID, ref.TurnID)
	}
	token, err := t.signer.Mint(ref.LaunchID+boxSubjectSep+ref.TurnID, RunTokenTTL)
	if err != nil {
		return "", fmt.Errorf("gitlet: the box credential of launch %s of run %s: %w", ref.LaunchID, ref.TurnID, err)
	}
	return token, nil
}

// ErrNoBoxRun reports a ref [BoxTokens.Mint] refuses: one no run has, its
// launch id no UUID, or its turn id empty, longer than maxBoxTurnBytes or
// holding `.`, `~` or whitespace.
var ErrNoBoxRun = errors.New("gitlet: no run has this launch and turn, so no box credential names them")

// Verify answers the run a token names, false for one forged, expired,
// malformed, signed under a key this node's ring does not hold, or naming a
// launch id that is not a UUID.
func (t *BoxTokens) Verify(token string) (BoxRunRef, bool) {
	subject := t.signer.Validate(token)
	if subject == "" {
		return BoxRunRef{}, false
	}
	launch, turn, ok := strings.Cut(subject, boxSubjectSep)
	ref := BoxRunRef{TurnID: turn, LaunchID: launch}
	if !ok || !validRef(ref) {
		return BoxRunRef{}, false
	}
	return ref, true
}

// validRef reports whether ref names a run's job: a launch id that is a UUID
// in its canonical spelling, and a turn id that is present, at most
// maxBoxTurnBytes, and free of `.`, the subject's separator and whitespace.
func validRef(ref BoxRunRef) bool {
	if id, err := uuid.Parse(ref.LaunchID); err != nil || id.String() != ref.LaunchID {
		return false
	}
	return ref.TurnID != "" && len(ref.TurnID) <= maxBoxTurnBytes &&
		!strings.ContainsAny(ref.TurnID, "."+boxSubjectSep) && !strings.ContainsFunc(ref.TurnID, unicode.IsSpace)
}
