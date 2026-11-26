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

	"github.com/crewlet/crewlet/internal/objstore"
)

// HandedSecrets is every value a launch handed its box out of a credential
// position ([handedSecrets]), which a capture of the box's session (session.go)
// scrubs out by value, and where its seeded logins live. Nothing else goes, since
// a transcript scrubbed of an ordinary word misleads the next launch: the seat's
// handle and email, a setting written in the document (internal/redact's shapes
// take a credential written there), an MCP server's name ([withoutServerNames]),
// a reference's unresolved text and a setup step's script or file all stay.
// Taken from the launch's own request, since read again at a capture a rotated or
// renamed secret resolves to what the box was not given; kept sealed under the
// fleet keyring ([SessionKeep.SealHanded]) beside the run's row, never on it
// ([PendingStore.Handed]): the coordination store is unencrypted, a capture may
// run on any node, and every git request and listing reads the row whole.
type HandedSecrets struct {
	// Values is every value from a credential position, whole and by every
	// JSON string ([secretValues]), the strings of each seeded login, and
	// every token a refresh recorded since ([HandedSecrets.record]): ONE LIST,
	// since an older build's scrub decodes this field alone.
	Values []string `json:"values"`

	// Logins maps each seeded login file, relative to the box's home
	// ([Spec.CredentialFiles]), to the engine-host file it was seeded from.
	// A capture reads both again ([Coordinator.scrubValues]): the harness in
	// the box and the entry's calls on the host refresh a login while the run
	// works, minting values the launch never saw.
	Logins map[string]string `json:"logins,omitempty"`

	// Expires is when each value a refresh recorded lapses, by [expiryKey]:
	// its issuer's expiry, [handedLapse] past which it is dropped
	// ([HandedSecrets.drop]). A value with none, as every launch-handed one,
	// is kept as long as the set. KEYED BY A DIGEST, so an expiry costs a few
	// dozen bytes rather than the value again; BESIDE Values, so an older
	// build still decodes, scrubs and carries forward every value, merely
	// never dropping one.
	Expires map[string]time.Time `json:"expires,omitempty"`

	// Extra is every key of the set this build does not know, kept verbatim
	// through every write this build makes (a renewal's record, a launch's
	// carry via [HandedSecrets.union]), so a later build's fact survives this
	// build's poll on a mixed fleet (wirekeys.go). Filled by the decode, never
	// by a caller.
	Extra map[string]json.RawMessage `json:"-"`
}

// MaxHandedBytes bounds the secrets one launch keeps, encoded, before they are
// sealed: its own, those carried from earlier jobs in the same box, and the
// tokens a refresh renewed there since.
//
// 80 KiB: a box is handed a few kibibytes (tokens, MCP headers, a deploy key, a
// login document of a kibibyte or two), so 48 KiB is room for a dozen key files
// across a turn's relaunches; and a run's renewals, which the drop holds to
// fourteen AWS sessions of ~2 KiB each (one every ten minutes, kept fifteen
// minutes and [handedLapse] past it), 28 KiB, rounded up to a 16 KiB step. A
// launch past it keeps none, and a set renewals take past it is marked
// incomplete ([PendingStore.AddHanded]); either way its captures are refused,
// never scrubbed of only some values.
const MaxHandedBytes = 80 << 10

// handedLapse is how long past its expiry, by the dropping node's clock, a token
// a refresh recorded stays in the set ([HandedSecrets.drop]): twice
// [objstore.AssumedClockSkew], the most that clock and the issuer's, each held
// within the bound, stand apart. So a value is dropped only once its issuer's
// clock is past the expiry too, and a lapsed token opens nothing. It is what
// bounds a long run: an AWS session renewed every ten minutes, lapsing fifteen
// after, would pass [MaxHandedBytes] within hours undropped.
const handedLapse = 2 * objstore.AssumedClockSkew

// expiryKey is what [HandedSecrets.Expires] holds value's expiry under: the
// first sixteen bytes of its SHA-256 in hex, 128 bits, so two values of one set
// never share a key.
func expiryKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

// ErrHandedUnknown is a capture refused because the secrets its box was handed
// cannot be known: the run keeps none for the job (its launch could not seal
// them, or their write did not land), or a seal this node cannot open. Such a
// session is refused rather than kept scrubbed of less.
var ErrHandedUnknown = errors.New("sandbox: the secrets the engine handed the box cannot be read back")

// ErrHandedIncomplete is a job's set marked as holding less than its box does
// ([handedSet.Incomplete]): every capture of the job is refused from then on,
// wrapped in [ErrHandedUnknown], saying why.
var ErrHandedIncomplete = errors.New("sandbox: the secrets kept for the job's scrub hold less than its box does")

// ErrJobNotRunning is a token a refresh was about to write into a box, left
// unrecorded because the run's record no longer holds the job running with its
// ending undecided ([PendingStore.AddHanded]): the poll is a moment old and the
// job is over or ending, so nothing more is written into the box.
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

// handedSecrets is what req hands its box out of a credential position: what
// [LaunchRequest.Secrets] names (every `${...}` it resolved, every variable whose
// name says credential, the entry's sign-in, and the Gitlet credential the launch
// writes into the box, [LaunchEnv.GitAccess]), every MCP server's env, headers and
// resolved references, the strings of each seeded login, and the run's export
// token in env's telemetry endpoint ([withTelemetry]), minted for this job after
// the request was built. Each is kept whole and by its JSON strings
// ([secretValues]).
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
	handed.Values = dedupe(withoutServerNames(values, req.MCPServers))
	return handed
}

// withoutServerNames is values less every one that is exactly the name of an
// MCP server the launch declares, whatever position also holds it: the document
// states the name in every revision, and it is part of every tool name of that
// server (`mcp__<server>__<tool>`), so scrubbing it would rename a tool
// definition the harness restores outside the provider's pattern, which then
// refuses every request of the session.
func withoutServerNames(values []string, servers map[string]MCPServer) []string {
	return slices.DeleteFunc(values, func(v string) bool {
		_, declared := servers[v]
		return declared
	})
}

// union is h with what carried holds that h does not: the secrets a job before
// this one handed the same box ([PriorBox.Handed]), or the tokens a launch's
// refresh wrote into it before its job started, with their values, their
// logins, their lapses and the keys a later build wrote into either
// ([HandedSecrets.Extra], h's where both hold one). A value either holds for
// good — one a launch handed — is held for good; one both hold until a lapse
// lapses at the later of the two.
func (h HandedSecrets) union(carried HandedSecrets) HandedSecrets {
	out := HandedSecrets{Values: dedupe(append(slices.Clone(h.Values), carried.Values...))}
	if len(h.Logins)+len(carried.Logins) > 0 {
		out.Logins = maps.Clone(carried.Logins)
		if out.Logins == nil {
			out.Logins = map[string]string{}
		}
		maps.Copy(out.Logins, h.Logins)
	}
	if len(h.Extra)+len(carried.Extra) > 0 {
		out.Extra = maps.Clone(carried.Extra)
		if out.Extra == nil {
			out.Extra = map[string]json.RawMessage{}
		}
		maps.Copy(out.Extra, h.Extra)
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

// loginValues is every string a login document holds (a harness's credentials
// file, a login export's output), however its harness names its tokens and keys;
// the rest is a scope or plan name a transcript loses nothing by. A Google
// service-account key keeps its format's constants ([jsonStrings]). A document
// that is not JSON is taken whole, a line at a time, as a setup step's file is.
func loginValues(doc []byte) []string {
	strs, isJSON := jsonStrings(doc)
	if !isJSON {
		return []string{string(doc)}
	}
	return strs
}

// secretValues is value as the scrub takes it out: whole, then, where it is a
// JSON document, every string it holds ([jsonStrings]), since a transcript prints
// either the file or one string read out of it: an AWS session reaches its box as
// the document its credential_process prints (internal/engine's awstoken.go), and
// its token printed bare matches no whole document and no shape. EVERY string, as
// only member names tell a credential from a setting and a secret under an unknown
// name would be kept, so a referenced key's project ID and client email go too (a
// setting written in the document or its own variable never does); but for
// [serviceAccountConstant]. Names, numbers, booleans and nulls are no strings,
// and the scrub's 8-byte floor (internal/sandbox/executor's minScrubBytes) keeps
// a `true` or a short word: it applies to every value the scrub is handed, so
// not here.
func secretValues(value string) []string {
	strs, _ := jsonStrings([]byte(value))
	return append([]string{value}, strs...)
}

// jsonStrings is every string the JSON value doc holds — an object's members
// by name, an array's in order, depth first — and whether doc is one JSON value
// at all; of a Google service-account key, every string but the values of the
// members its format fixes ([serviceAccountConstant]).
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
	members, isObject := tree.(map[string]any)
	if !isObject || members["type"] != serviceAccountType {
		walk(tree)
		return out, true
	}
	for _, key := range slices.Sorted(maps.Keys(members)) {
		if !serviceAccountConstant(key) {
			walk(members[key])
		}
	}
	return out, true
}

// serviceAccountType is the `type` a Google service-account key declares
// itself by, and the only one [jsonStrings] reads a format's constants of.
const serviceAccountType = "service_account"

// serviceAccountConstant reports whether member is one whose value Google's
// service-account key format fixes: its `type`, the endpoints its token is
// asked of and its certificates are read from, and its universe. These are the
// same in every key of a universe, published, and open nothing; scrubbed, they
// would blank every googleapis.com URL the seat's tools print. One closed list,
// for one format the engine integrates first-class, decided on the key's
// declared `type` alone. The rest of the key (its private key and that key's id,
// client email and id, project, and client certificate URL, which names the
// email) goes as any document's strings do, and a document of any other format
// keeps no constant, its `type` included.
func serviceAccountConstant(member string) bool {
	switch member {
	case "type", "auth_uri", "token_uri", "auth_provider_x509_cert_url", "universe_domain":
		return true
	}
	return false
}

// boxLoginPath is where a login seeded at rel relative to the box's home lives
// in box.
func boxLoginPath(box Sandbox, rel string) string {
	return path.Join(box.Home(), rel)
}

// sealHanded is what a launch keeps beside its row ([PendingStore.Handed]): the
// secrets it handed its box and the tokens its refresh wrote there before the job
// started (renewed, [CoordinatorOptions.Refresh]), sealed, those lapsed past
// [handedLapse] dropped. A job continuing in its turn's kept box (continues)
// runs no setup and resumes the previous job's session, which holds that job's
// lines, so it also carries what prior's job and those before it handed the box
// ([PriorBox.Handed]). It is "" where none can be kept: a company that keeps no
// session ([Coordinator.capturing]), or, logged and refusing every capture of the
// job rather than one scrubbed of less, a continued box whose record says nothing
// of its set or holds a seal this node cannot open, secrets past
// [MaxHandedBytes], or a keyring that cannot seal.
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

// RecordHanded records value, a token the engine's poll (internal/engine's
// issued.go, via [WaiterOptions.Record]) is about to write into run's box,
// lapsing at expires, in the job's sealed set ([PendingStore.AddHanded]) BEFORE
// it is written: re-read at a capture it would be a fresh issue, and no shape
// names a bare AWS secret key. Nil means write it (recorded now or before, or
// the set already refuses every capture); an error means do not, and the next
// poll asks again (the store did not answer, or [ErrJobNotRunning]). A set that
// cannot take it ([Coordinator.addHanded]) is marked incomplete
// ([ErrHandedIncomplete]), logged, and nil returned all the same: the run needs a
// good token more than a kept session. A still-working job's capture reads the
// set before its pack ([Coordinator.takeCapture]), so a token renewed mid-pack
// is caught only by a shape.
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
