package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// THE SECRETS A LAUNCH HANDS ITS BOX, kept for the scrub its session leaves
// through (session.go).
//
// # Every credential position, and only those
//
// A kept session must carry no secret the engine put in its box, and must keep
// everything else, since a transcript scrubbed of an ordinary word is one the
// next launch reads wrong. So the scrub takes out by value what sits in a
// CREDENTIAL POSITION of the launch:
//
//   - every value a `${...}` reference resolved to — in the run's
//     environment, in a setup file that is one reference whole, in an MCP
//     server's url — what the operator kept out of the document
//     ([LaunchRequest.Secrets], [MCPServer.Secrets]);
//   - every variable of the run's environment whose name says it carries a
//     credential, and the entry's sign-in, written or referenced alike
//     ([LaunchRequest.Secrets]);
//   - every MCP server's env and headers, written or referenced — where a
//     tool server's credentials are declared;
//   - the run's own export token, in its telemetry endpoint, and its
//     credential on Gitlet ([GitTokenVar]);
//   - and the login the box was seeded with.
//
// NOT the engine's identity facts (the seat's handle and email, which a
// transcript names in every commit), a setting written in the document, the
// unresolved text of a reference, or a setup step's script or file: none is a
// secret, and scrubbed by value each was lost from every kept session — the
// seat's own name read as a redacted secret throughout — and a setup step's
// large file put a launch past [MaxHandedBytes], which refused every capture of
// the seat. A credential written in the document is the shape pass's
// (internal/redact): the document keeps it in every revision anyway.
//
// # Taken at the launch, from what the box was given
//
// Read again at the capture, off the company the node runs THEN, they were not
// what the box was given: a rotated secret resolved to its new value while the
// box held the old one, and an entry renamed since resolved to nothing at all,
// so the scrub took out less than the box was handed and said nothing. So the
// launch collects them from the very request it starts the job with
// ([handedSecrets]), and the run keeps them SEALED under the fleet keyring
// ([SessionKeep.SealHanded]), bound to the turn, BESIDE its row and named for
// the job ([PendingStore.Handed]): the coordination store is not encrypted, a
// capture may be made on any node whose host the box is on, and the row, which
// every git request of a coding box and every listing reads whole, is kept
// small of what only a capture and the next launch read.
//
// # Carried forward where a box is kept
//
// A job that continues in the box the turn's previous job ran in — its
// reviewer's send-back, a person's answer — runs no setup, and resumes the
// previous job's session, which holds that job's lines. So its launch carries
// the previous job's sealed set forward and seals both together
// ([PriorBox.Handed]): a secret rotated while the box waited, or a step removed
// since, was otherwise handed to the box by the first launch and scrubbed by no
// later capture of it.
//
// # And read again, or recorded, where they change
//
// A login is refreshed while the run works — in the box, by its harness, and
// on the engine host, by the entry's own calls — and each refresh mints values
// the launch never saw. So the sealed set also keeps where each seeded login lives,
// and the capture reads it again in the box and on the host before it scrubs
// ([Coordinator.scrubValues]).
//
// A token the engine issues the seat is RENEWED in a running box's files by the
// completion poll (internal/engine's issued.go): an AWS session every ten
// minutes, a GitHub or Google Cloud token about once an hour. Read again at a
// capture it would be another issue, never the token the box holds; and the
// shapes do not catch every one — an AWS session's secret key and its token,
// printed bare, have none. So the value is RECORDED IN THE JOB'S SEALED
// SET BEFORE IT IS WRITTEN: on the poll, beside the job's row
// ([Coordinator.RecordHanded], [PendingStore.AddHanded]); at the launch of a job
// that continues in its turn's kept box, whose files the launch brings up to
// date before the job starts, in the set that job is sealed with
// ([Coordinator.sealHanded]). A value that cannot be recorded — the store did
// not answer, or the row no longer holds the job running — is not written, and
// the next poll asks again. So the set holds every value the box was given
// before the box holds it, and a capture made once its job has stopped reading
// files holds every value the box could hold.
//
// IN [HandedSecrets.Values] ITSELF, where an older build's scrub, which decodes
// that alone, takes the renewed values too; when each one lapses rides beside
// them ([HandedSecrets.Expires]), which only this build reads, and a value is
// dropped [handedLapse] past it, so a long run's renewals do not grow the set
// without bound. A set still past [MaxHandedBytes] after the drop would hold
// less than its box, and is MARKED INCOMPLETE instead ([handedSet.Incomplete]):
// every later capture of the job is refused, as one of a launch past the bound
// is, and the token is written all the same, since the run needs a good token
// more than it needs a kept session.
//
// # Whole, and by every string a document holds
//
// A value is taken out whole and, where it is a JSON document, by every string
// it holds ([secretValues]), as a login is ([loginValues]): an AWS session
// reaches its box as the document its credential_process prints, and a
// transcript prints the session's token bare, which the whole document never
// matches and no shape names.
//
// # A capture that cannot know them keeps nothing
//
// A job with no sealed secrets beside its row — a launch whose secrets could
// not be sealed, or whose write of them did not land — or a seal this node
// cannot open is a
// session that cannot be scrubbed of what the engine put in its box, and it is
// refused rather than kept scrubbed of less.

// HandedSecrets is every value a launch handed its box that a capture of the
// box's session scrubs out by value, and where its seeded logins live.
type HandedSecrets struct {
	// Values is every value the launch handed the box out of a credential
	// position (see the file comment), each whole and by every string a
	// document of them holds ([secretValues]), the strings its seeded login
	// held, and every token a refresh wrote into the box since
	// ([HandedSecrets.record]): ONE LIST, the one an older build's scrub
	// reads.
	Values []string `json:"values"`

	// Logins maps each login file the box was seeded with — its path
	// relative to the box's home ([Spec.CredentialFiles]) — to the file on
	// the engine host it was seeded from, both of which are read again at a
	// capture.
	Logins map[string]string `json:"logins,omitempty"`

	// Expires is when each value a refresh recorded lapses, by
	// [expiryKey] of the value: the expiry its issuer gave the token it
	// belongs to, past which the value is dropped ([HandedSecrets.drop]). A
	// value with none is kept for as long as the set is, which is every value
	// a launch handed. KEYED BY A DIGEST, not by the value, so the expiries
	// cost the set a few dozen bytes a value rather than every value twice;
	// and BESIDE Values rather than in it, so an older build, which knows
	// nothing of a lapse, still decodes and scrubs every value, and carries
	// every one forward — never dropped, which only costs it room.
	Expires map[string]time.Time `json:"expires,omitempty"`
}

// MaxHandedBytes bounds the secrets one launch keeps, encoded, before
// they are sealed — its own, those carried from the jobs before it in the
// same box, and the tokens a refresh renewed in the box since.
//
// SIXTY-FOUR KIBIBYTES. A box is handed a handful of tokens, a few MCP servers'
// headers, a deploy key a setup step writes from a reference, and a login
// document of a kibibyte or two: a few kibibytes in all, a key file the
// largest single value. Sixty-four is room for a dozen keys beside everything
// else, carried across a turn's relaunches — kept beside the run's row, never
// on it, since even this much on the row would ride every read of it. A launch
// handed more keeps none, and its captures are refused saying so, rather than
// a set grown without bound or a scrub of only some of them.
//
// AND A RUN'S RENEWALS, which the drop holds to the last hour and a quarter of
// them ([handedLapse]): an AWS session, the largest and most often renewed, is
// a document of about a kibibyte and a kibibyte more of the strings it holds,
// renewed every ten minutes and lapsing fifteen after its issue, so about eight
// are held at once — some eighteen kibibytes — and a GitHub or a Google Cloud
// token a few hundred bytes more. A set the renewals take past the bound is
// marked incomplete, as a launch past it keeps none ([PendingStore.AddHanded]).
const MaxHandedBytes = 64 << 10

// handedLapse is how long past its expiry a token a refresh recorded stays in
// the set ([HandedSecrets.drop]).
//
// AN HOUR. A lapsed token opens nothing, so a session that keeps one keeps no
// secret; an hour is far past any clock's error between the issuer, the node
// that recorded the token and the node that drops it, so nothing still good is
// dropped. And it is what bounds the set on a long run: an AWS session renewed
// every ten minutes is 144 renewals a day, a kibibyte or two each, past
// [MaxHandedBytes] within a few hours were none dropped, and about eight at
// once with the drop.
const handedLapse = time.Hour

// expiryKey is what [HandedSecrets.Expires] holds value's expiry under: the
// first sixteen bytes of its SHA-256, in hex — a hundred and twenty-eight bits,
// so two values of one set never share a key.
func expiryKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

// ErrHandedUnknown is a capture refused because the secrets its box was
// handed cannot be known: the run keeps none for the job, or a seal this node
// cannot open.
var ErrHandedUnknown = errors.New("sandbox: the secrets the engine handed the box cannot be read back")

// ErrHandedIncomplete is a job's set marked as holding less than its box does
// ([handedSet.Incomplete]): every capture of the job is refused from then on,
// wrapped in [ErrHandedUnknown], saying why.
var ErrHandedIncomplete = errors.New("sandbox: the secrets kept for the job's scrub hold less than its box does")

// ErrJobNotRunning is a token a refresh was about to write into a box that
// was not recorded because the run's record no longer holds the job running,
// its ending undecided ([PendingStore.AddHanded]): the poll that read it is a
// moment old, and the job is over or ending. Nothing more is written into the
// box for it.
var ErrJobNotRunning = errors.New("sandbox: the run's record no longer holds the job running")

// Why a job's set is marked as holding less than its box does
// ([handedSet.Incomplete]): never a value, never an error's words, since the
// mark sits unsealed beside the row.
const (
	// handedUnkept: the poll found no set for the job to add a renewed token
	// to — its launch sealed none, or its write of one did not land.
	handedUnkept = "the job's launch kept no set of what it handed its box beside the run's record, " +
		"so the token the poll renewed there is in none"
	// handedUnopened: the set the token was to join could not be opened, or
	// read once opened, on the node that renewed it.
	handedUnopened = "the set could not be opened on the node that renewed a token in the box"
	// handedOverflowed: the token took the set past its bound with every
	// lapsed value dropped.
	handedOverflowed = "the tokens renewed in the box took the set past the bound a run keeps for its scrub"
	// handedUnsealable: the grown set could not be sealed again.
	handedUnsealable = "the set could not be sealed again on the node that renewed a token in the box"
)

// handedSecrets is what req hands its box out of a credential position — see
// the file comment — env being the environment the job is started with
// ([withTelemetry]), whose run export token and Gitlet credential
// ([GitTokenVar], minted for the launch by [LaunchRequest.RunEnv]) are the
// secrets of it no request field names: both minted for this job alone, after
// the request was built. Each value is kept whole and by every string it holds
// where it is a JSON document ([secretValues]): a seat's AWS session is handed
// as one, in the file its credential_process reads.
func handedSecrets(req LaunchRequest, env map[string]string) HandedSecrets {
	var values []string
	add := func(vs ...string) {
		for _, v := range vs {
			for _, s := range secretValues(v) {
				if strings.TrimSpace(s) != "" {
					values = append(values, s)
				}
			}
		}
	}
	add(req.Secrets...)
	if _, token, ok := strings.Cut(env[otelBoxEndpointVar], otelTokenPath); ok {
		add(token)
	}
	add(env[GitTokenVar])
	for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
		server := req.MCPServers[name]
		for _, key := range slices.Sorted(maps.Keys(server.Env)) {
			add(server.Env[key])
		}
		for _, key := range slices.Sorted(maps.Keys(server.Headers)) {
			add(server.Headers[key])
		}
		add(server.Secrets...)
	}
	handed := HandedSecrets{Logins: maps.Clone(req.Spec.CredentialFiles)}
	for _, rel := range slices.Sorted(maps.Keys(handed.Logins)) {
		// THE LOGIN AS IT WAS SEEDED: read here, where the box's copy
		// came from, since the host's may be refreshed before a capture
		// reads it again.
		raw, err := os.ReadFile(handed.Logins[rel]) //nolint:gosec // the entry's own login, at the path its harness keeps it
		if err == nil {
			add(loginValues(raw)...)
		}
	}
	handed.Values = dedupe(values)
	return handed
}

// union is h with what carried holds that h does not: the secrets a job before
// this one handed the same box ([PriorBox.Handed]), or the tokens a launch's
// refresh wrote into it before its job started, with their values, their
// logins and their lapses. A value either holds for good — one a launch handed
// — is held for good; one both hold until a lapse lapses at the later of the
// two.
func (h HandedSecrets) union(carried HandedSecrets) HandedSecrets {
	out := HandedSecrets{Values: dedupe(append(slices.Clone(h.Values), carried.Values...))}
	if len(h.Logins)+len(carried.Logins) > 0 {
		out.Logins = maps.Clone(carried.Logins)
		if out.Logins == nil {
			out.Logins = map[string]string{}
		}
		maps.Copy(out.Logins, h.Logins)
	}
	inH, inCarried := valueSet(h.Values), valueSet(carried.Values)
	for _, v := range out.Values {
		key := expiryKey(v)
		hAt, hLapses := h.Expires[key]
		cAt, cLapses := carried.Expires[key]
		switch {
		case inH[v] && !hLapses, inCarried[v] && !cLapses:
			// HELD FOR GOOD on one side, so for good.
		case inH[v] && inCarried[v]:
			out.setLapse(key, laterOf(hAt, cAt))
		case inH[v]:
			out.setLapse(key, hAt)
		default:
			out.setLapse(key, cAt)
		}
	}
	return out
}

// record adds value — a token a refresh is about to write into the box, which
// its issuer said lapses at expires — whole and by every string it holds
// ([secretValues]), and reports whether the set changed. Each lapses at
// expires, or at a later expiry it was recorded with before; a value the set
// already holds for good — one a launch handed — is never given one, and a
// zero expires, an issuer that named none, holds the value for good too.
func (h *HandedSecrets) record(value string, expires time.Time) bool {
	held := valueSet(h.Values)
	changed := false
	for _, v := range secretValues(value) {
		if strings.TrimSpace(v) == "" {
			continue
		}
		key := expiryKey(v)
		at, lapses := h.Expires[key]
		switch {
		case !held[v]:
			h.Values = append(h.Values, v)
			held[v] = true
			// AN EXPIRY THAT NAMED NO VALUE, a set's leftover, is not this
			// value's: written over, or removed for one held for good.
			h.setLapse(key, expires)
			changed = true
		case !lapses:
			// HELD FOR GOOD: what a launch handed is never dropped.
		case expires.IsZero():
			h.setLapse(key, expires)
			changed = true
		case expires.After(at):
			h.setLapse(key, expires)
			changed = true
		}
	}
	return changed
}

// drop removes every value that lapsed more than [handedLapse] before now, and
// every expiry that names no value, and reports whether it removed a value.
func (h *HandedSecrets) drop(now time.Time) bool {
	if len(h.Expires) == 0 {
		return false
	}
	kept := make([]string, 0, len(h.Values))
	named := make(map[string]bool, len(h.Expires))
	dropped := false
	for _, v := range h.Values {
		key := expiryKey(v)
		if at, lapses := h.Expires[key]; lapses && now.After(at.Add(handedLapse)) {
			dropped = true
			continue
		}
		kept = append(kept, v)
		named[key] = true
	}
	h.Values = kept
	for key := range h.Expires {
		if !named[key] {
			delete(h.Expires, key)
		}
	}
	if len(h.Expires) == 0 {
		h.Expires = nil
	}
	return dropped
}

// setLapse records that the value whose key is key lapses at at, or that it
// is held for good where at is zero.
func (h *HandedSecrets) setLapse(key string, at time.Time) {
	if at.IsZero() {
		delete(h.Expires, key)
		return
	}
	if h.Expires == nil {
		h.Expires = map[string]time.Time{}
	}
	h.Expires[key] = at.UTC()
}

// laterOf is the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// valueSet is values as a set.
func valueSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// dedupe is values, each once, in the order first given.
func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// encode is the secrets as the row's seal covers them, refused past
// [MaxHandedBytes].
func (h HandedSecrets) encode() ([]byte, error) {
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxHandedBytes {
		return nil, fmt.Errorf("sandbox: the secrets the box was handed are %d bytes encoded, "+
			"past the %d a run keeps for its session's scrub", len(raw), MaxHandedBytes)
	}
	return raw, nil
}

// decodeHanded reads what [HandedSecrets.encode] wrote.
func decodeHanded(raw []byte) (HandedSecrets, error) {
	var h HandedSecrets
	if err := json.Unmarshal(raw, &h); err != nil {
		return HandedSecrets{}, fmt.Errorf("sandbox: the secrets the box was handed do not decode: %w", err)
	}
	return h, nil
}

// loginValues is every string a login document holds — a harness's
// credentials file, the document its login export printed — for the scrub: its
// tokens and its keys, however its harness spells their names. Every string,
// not only those under a credential's key; what else such a document holds is a
// scope or a plan's name, whose occurrences a transcript loses nothing by. A
// document that is not JSON is taken whole, a line at a time, as a setup step's
// file is.
func loginValues(doc []byte) []string {
	strs, isJSON := jsonStrings(doc)
	if !isJSON {
		return []string{string(doc)}
	}
	return strs
}

// secretValues is value as the scrub takes it out: whole, then, where it is a
// JSON document, every string it holds ([jsonStrings]).
//
// WHOLE AND BY ITS STRINGS, because a transcript holds either: the whole where
// the run printed the file it was written to, a string alone where it printed
// what it read out of it. An AWS session reaches its box as the document its
// credential_process prints (internal/engine's awstoken.go), and its session
// token printed alone is matched by no whole document and named by no shape.
// What else such a document holds goes with it — an AWS session's expiry, as
// the instant it is written in — which a transcript loses as little by as by a
// login's scopes; its member names are no strings it holds, and a string under
// eight bytes is left to the shapes, as every value is.
func secretValues(value string) []string {
	strs, _ := jsonStrings([]byte(value))
	return append([]string{value}, strs...)
}

// jsonStrings is every string the JSON value doc holds — an object's members
// by name, an array's in order, depth first — and whether doc is one JSON value
// at all.
func jsonStrings(doc []byte) ([]string, bool) {
	var tree any
	if err := json.Unmarshal(doc, &tree); err != nil {
		return nil, false
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(t)) {
				walk(t[key])
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(tree)
	return out, true
}

// boxLoginPath is where a login seeded at rel relative to the box's home lives
// in box.
func boxLoginPath(box Sandbox, rel string) string {
	return path.Join(box.Home(), rel)
}

// sealHanded is what a launch keeps beside its row of the secrets it handed its box
// ([PendingStore.Handed]): sealed — with, where the job continues in the box the
// job before it ran in (continues), what prior's job and those before it handed
// that box ([PriorBox.Handed]), and the tokens the launch's refresh wrote into
// that box before the job started (renewed, [CoordinatorOptions.Refresh]),
// those lapsed past [handedLapse] dropped — or "" where they cannot be: a
// company that keeps no session ([Coordinator.capturing]), a box continued
// whose record says nothing of what it was handed or holds a seal this node
// cannot open, secrets past [MaxHandedBytes], a keyring that cannot seal. Each
// but the first is logged, and refuses every capture of the job rather than
// one scrubbed of less than its box holds.
func (c *Coordinator) sealHanded(ctx context.Context, req LaunchRequest, env map[string]string,
	prior PriorBox, continues bool, renewed HandedSecrets) string {

	if !c.capturing() {
		return ""
	}
	handed := handedSecrets(req, env).union(renewed)
	var err error
	if continues {
		handed, err = c.carryHanded(req.Turn.TurnID, handed, prior.Handed)
	}
	var raw []byte
	if err == nil {
		handed.drop(c.now())
		raw, err = handed.encode()
	}
	if err == nil {
		var sealed string
		if sealed, err = c.keep.SealHanded(req.Turn.TurnID, raw); err == nil {
			return sealed
		}
	}
	log.WarnContext(ctx, "executor_session_scrub_unsealed", "turn_id", req.Turn.TurnID, "error", err.Error(),
		"detail", "the secrets the launch handed its box could not be kept for its session's scrub, so no "+
			"capture of this job is kept")
	return ""
}

// carryHanded is handed with what the jobs before it handed the box it
// continues in, from their sealed set (carried), or why that cannot be known.
func (c *Coordinator) carryHanded(turnID string, handed HandedSecrets, carried string) (HandedSecrets, error) {
	if carried == "" {
		return HandedSecrets{}, errors.New("the job continues in a box whose record keeps nothing of " +
			"what the jobs before it handed the box, so its session cannot be scrubbed of them")
	}
	raw, err := c.keep.OpenHanded(turnID, carried)
	if err != nil {
		return HandedSecrets{}, fmt.Errorf("the secrets the jobs before it handed the box it continues in "+
			"could not be opened to carry them forward: %w", err)
	}
	before, err := decodeHanded(raw)
	if err != nil {
		return HandedSecrets{}, err
	}
	return handed.union(before), nil
}

// RecordHanded records value — a token the poll's refresh of run's box is
// about to write there, which its issuer said lapses at expires — in the
// sealed set beside the job's row that every capture of the job's session is
// scrubbed by ([PendingStore.AddHanded]): the waiter's [WaiterOptions.Record]
// in the engine, the recorder a poll hands its refresh ([RecordHanded]).
//
// NIL IS A VALUE THE REFRESH MAY WRITE: recorded now, recorded by an earlier
// poll whose write did not land, or one the job's set already refuses every
// capture over. An ERROR IS A VALUE THE REFRESH DOES NOT WRITE: the store did
// not answer, and the next poll asks again; or [ErrJobNotRunning], a poll a
// moment old whose job is over or ending.
//
// ADDED THROUGH THE SET'S OWN SEAL ([Coordinator.addHanded]), and where the
// set cannot take it — this node cannot open, read or seal it again, or the
// token takes it past [MaxHandedBytes] with every lapsed value dropped — the
// set is MARKED INCOMPLETE rather than left without it, logged once here, and
// the value is written all the same: the run needs a good token more than a
// kept session, and a set that holds less than its box refuses every later
// capture of the job ([ErrHandedIncomplete]).
func (c *Coordinator) RecordHanded(ctx context.Context, run PendingRun, value string, expires time.Time) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	// AS THE LAST ASK OF add LEFT THEM, which the store asks again on every
	// write it loses: why the set it answered was marked, and what failed.
	var why string
	var cause error
	recorded, err := c.pending.AddHanded(ctx, run, func(sealed string) (string, string) {
		var next string
		next, why, cause = c.addHanded(run.TurnID, sealed, value, expires)
		return next, why
	})
	switch {
	case err != nil:
		return fmt.Errorf("sandbox: the token could not be recorded for the scrub of the run's sessions: %w", err)
	case !recorded:
		return ErrJobNotRunning
	}
	if why != "" {
		attrs := []any{"turn_id", run.TurnID, "launch", run.LaunchID, "reason", why,
			"detail", "the set of what the job's box was handed no longer holds every value the box holds, so " +
				"every later capture of its session is refused rather than kept scrubbed of less; the renewed " +
				"token is written into the box all the same"}
		if cause != nil {
			attrs = append(attrs, "error", cause.Error())
		}
		log.WarnContext(ctx, "executor_session_scrub_incomplete", attrs...)
	}
	return nil
}

// addHanded is sealed — run's job's set as it stands beside its row, sealed
// for turnID — with value recorded in it ([HandedSecrets.record]) and every
// value lapsed past [handedLapse] dropped, sealed again: or sealed itself where
// neither changed it, so the store writes nothing; or "", why the set can no
// longer hold every value its box holds ([handedSet.Incomplete]) and the error
// that says so, for the log alone.
func (c *Coordinator) addHanded(turnID, sealed, value string, expires time.Time) (string, string, error) {
	if c.keep == nil {
		return "", handedUnopened, errors.New("this coordinator keeps no sessions' seals")
	}
	raw, err := c.keep.OpenHanded(turnID, sealed)
	if err != nil {
		return "", handedUnopened, err
	}
	handed, err := decodeHanded(raw)
	if err != nil {
		return "", handedUnopened, err
	}
	recorded := handed.record(value, expires)
	dropped := handed.drop(c.now())
	if !recorded && !dropped {
		return sealed, "", nil
	}
	if raw, err = handed.encode(); err != nil {
		return "", handedOverflowed, err
	}
	next, err := c.keep.SealHanded(turnID, raw)
	if err != nil {
		return "", handedUnsealable, err
	}
	return next, "", nil
}
