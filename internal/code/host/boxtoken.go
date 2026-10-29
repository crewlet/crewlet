package host

import (
	"fmt"
	"strings"
	"testing"
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
// revocation.
//
// The run's ROW is the authority, read on every request ([Runs]): a run that
// ended, was reaped or launched again is refused the moment its row says so,
// whatever its token's expiry. A coding job has no wall-clock limit
// (sandbox.Limits), so an expiry shorter than any job a person would leave
// running fails that job's only durable output, its last push. Seven days —
// config.DefaultMinAge, the trim's replay window every other "a run's worth
// of history" bound in the engine is sized to — bounds the one case the row
// cannot: a row still live while its box is gone, which the completion poll
// settles within its give-up window and the seat's next holder's recovery
// reaps; a token copied out of a box is worth nothing past either. It is also
// the floor of a keyring rotation's last step: an old key dropped sooner
// refuses every running box's pushes (docs/concepts/secret-store.md).
const RunTokenTTL = 7 * 24 * time.Hour

// BoxRunRef names a run's ONE job: the row's key (the turn's run id) and the
// launch the token was minted for. A token naming the turn alone would keep
// admitting the first job's box after a second run_sandbox call of the same
// turn launched another on the row: the launch id, minted by the store at
// every launch, is what a superseded job's token stops matching.
type BoxRunRef struct {
	TurnID   string
	LaunchID string
}

// maxBoxTurnBytes bounds a token's turn id: every turn id is a UUIDv7 (36
// bytes), so 128 is past any the engine mints and keeps a token's subject — a
// path-safe word git sends in every request's header — short.
const maxBoxTurnBytes = 128

// boxSubjectSep joins a token's launch id to its turn id: the LAUNCH FIRST, a
// UUID that holds none, so the parse is a cut at the first separator whatever
// the turn id holds — mcpbridge's separator, for its reasons.
const boxSubjectSep = "~"

// BoxTokens mints and verifies box credentials. Safe for concurrent use.
type BoxTokens struct{ signer *runtoken.Signer }

// NewBoxTokens builds them over the fleet's keyring, under BoxKeyDomain,
// refusing one that cannot sign for the fleet (runtoken.ErrNoKeyring): a node
// that launched a run and the git node its box dials derive one key from it,
// so a token minted on one verifies on every other — and a rotation in flight
// verifies by every key the ring still holds. now nil is the wall clock.
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
// A ref no run has — a launch id that is not a UUID, a turn id empty, longer
// than maxBoxTurnBytes or holding `.`, `~` or whitespace — is the caller's bug
// (every launch id is a UUID the store mints, every turn id a UUIDv7): a test
// binary panics naming it, and a production one answers "", which no git node
// admits, rather than a token another run's parse could read.
func (t *BoxTokens) Mint(ref BoxRunRef) string {
	if why := invalidRef(ref); why != "" {
		if testing.Testing() {
			panic("host: BoxTokens.Mint: " + why)
		}
		return ""
	}
	return t.signer.Mint(ref.LaunchID+boxSubjectSep+ref.TurnID, RunTokenTTL)
}

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
	if !ok || invalidRef(ref) != "" {
		return BoxRunRef{}, false
	}
	return ref, true
}

// invalidRef is why ref names no run's job, or "".
func invalidRef(ref BoxRunRef) string {
	if id, err := uuid.Parse(ref.LaunchID); err != nil || id.String() != ref.LaunchID {
		return fmt.Sprintf("launch id %q is not a UUID in its canonical spelling", ref.LaunchID)
	}
	switch {
	case ref.TurnID == "":
		return "a run's token names its turn"
	case len(ref.TurnID) > maxBoxTurnBytes:
		return fmt.Sprintf("turn id of %d bytes, past %d", len(ref.TurnID), maxBoxTurnBytes)
	case strings.ContainsAny(ref.TurnID, "."+boxSubjectSep) || strings.ContainsFunc(ref.TurnID, unicode.IsSpace):
		return fmt.Sprintf("turn id %q holds a character a token's subject may not", ref.TurnID)
	}
	return ""
}
