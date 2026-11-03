package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
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
//     credential on the engine's own code host ([GitTokenVar]);
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
// # And read again where they change
//
// A login is refreshed while the run works — in the box, by its harness, and
// on the engine host, by the entry's own calls — and each refresh mints values
// the launch never saw. So the sealed set also keeps where each seeded login lives,
// and the capture reads it again in the box and on the host before it scrubs
// ([Coordinator.scrubValues]).
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
	// position (see the file comment), and the strings its seeded login
	// held.
	Values []string `json:"values"`

	// Logins maps each login file the box was seeded with — its path
	// relative to the box's home ([Spec.CredentialFiles]) — to the file on
	// the engine host it was seeded from, both of which are read again at a
	// capture.
	Logins map[string]string `json:"logins,omitempty"`
}

// MaxHandedBytes bounds the secrets one launch keeps, encoded, before
// they are sealed — its own and those carried from the jobs before it in the
// same box.
//
// SIXTY-FOUR KIBIBYTES. A box is handed a handful of tokens, a few MCP servers'
// headers, a deploy key a setup step writes from a reference, and a login
// document of a kibibyte or two: a few kibibytes in all, a key file the
// largest single value. Sixty-four is room for a dozen keys beside everything
// else, carried across a turn's relaunches — kept beside the run's row, never
// on it, since even this much on the row would ride every read of it. A launch
// handed more keeps none, and its captures are refused saying so, rather than
// a set grown without bound or a scrub of only some of them.
const MaxHandedBytes = 64 << 10

// ErrHandedUnknown is a capture refused because the secrets its box was
// handed cannot be known: the run keeps none for the job, or a seal this node
// cannot open.
var ErrHandedUnknown = errors.New("sandbox: the secrets the engine handed the box cannot be read back")

// handedSecrets is what req hands its box out of a credential position — see
// the file comment — env being the environment the job is started with
// ([withTelemetry]), whose run export token and code-host credential
// ([GitTokenVar], minted for the launch by [LaunchRequest.RunEnv]) are the
// secrets of it no request field names: both minted for this job alone, after
// the request was built.
func handedSecrets(req LaunchRequest, env map[string]string) HandedSecrets {
	var values []string
	add := func(vs ...string) {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				values = append(values, v)
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
// this one handed the same box ([PriorBox.Handed]), its values and its logins.
func (h HandedSecrets) union(carried HandedSecrets) HandedSecrets {
	out := HandedSecrets{Values: dedupe(append(slices.Clone(h.Values), carried.Values...))}
	if len(h.Logins)+len(carried.Logins) > 0 {
		out.Logins = maps.Clone(carried.Logins)
		if out.Logins == nil {
			out.Logins = map[string]string{}
		}
		maps.Copy(out.Logins, h.Logins)
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
		return nil, fmt.Errorf("sandbox: the secrets the launch handed its box are %d bytes encoded, "+
			"past the %d a run keeps for its session's scrub", len(raw), MaxHandedBytes)
	}
	return raw, nil
}

// decodeHanded reads what [HandedSecrets.encode] wrote.
func decodeHanded(raw []byte) (HandedSecrets, error) {
	var h HandedSecrets
	if err := json.Unmarshal(raw, &h); err != nil {
		return HandedSecrets{}, fmt.Errorf("sandbox: the secrets the launch handed its box do not decode: %w", err)
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
	var tree any
	if err := json.Unmarshal(doc, &tree); err != nil {
		return []string{string(doc)}
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
	return out
}

// boxLoginPath is where a login seeded at rel relative to the box's home lives
// in box.
func boxLoginPath(box Sandbox, rel string) string {
	return path.Join(box.Home(), rel)
}

// sealHanded is what a launch keeps beside its row of the secrets it handed its box
// ([PendingStore.Handed]): sealed — with, where the job continues in the box the
// job before it ran in (continues), what prior's job and those before it handed
// that box ([PriorBox.Handed]) — or "" where they cannot be: a company that
// keeps no session ([Coordinator.capturing]), a box continued whose record says
// nothing of what it was handed or holds a seal this node cannot open, secrets
// past [MaxHandedBytes], a keyring that cannot seal. Each but the first is
// logged, and refuses every capture of the job rather than one scrubbed of less
// than its box holds.
func (c *Coordinator) sealHanded(ctx context.Context, req LaunchRequest, env map[string]string,
	prior PriorBox, continues bool) string {

	if !c.capturing() {
		return ""
	}
	handed := handedSecrets(req, env)
	var err error
	if continues {
		handed, err = c.carryHanded(req.Turn.TurnID, handed, prior.Handed)
	}
	var raw []byte
	if err == nil {
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
