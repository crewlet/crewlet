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
// # Taken at the launch, from what the box was given
//
// A kept session must carry no value the engine put in its box: the run's
// environment — the seat's declared tokens, its setup steps' env, the entry's
// whole env and sign-in, the run's telemetry — its MCP servers' env and
// headers, its setup steps' files, and the login it was seeded with. Read again
// at the capture, off the company the node runs THEN, they were not that: a
// value written in the document rather than referenced was passed over, a
// rotated secret resolved to its new value while the box held the old one, and
// an entry renamed since resolved to nothing at all, so the scrub took out
// less than the box was handed and said nothing. So the launch collects them
// from the very request it starts the job with ([handedSecrets]), and the row
// keeps them SEALED under the fleet keyring ([SessionKeep.SealHanded]), bound
// to the turn: the coordination store is not encrypted, and a capture may be
// made on any node whose host the box is on.
//
// # And read again where they change
//
// A login is refreshed while the run works — in the box, by its harness, and
// on the engine host, by the entry's own calls — and each refresh mints values
// the launch never saw. So the row also keeps where each seeded login lives,
// and the capture reads it again in the box and on the host before it scrubs
// ([Coordinator.scrubValues]).
//
// # A capture that cannot know them keeps nothing
//
// A row with no sealed secrets — a launch whose secrets could not be sealed, or
// one this build's predecessors made — or a seal this node cannot open is a
// session that cannot be scrubbed of what the engine put in its box, and it is
// refused rather than kept scrubbed of less.

// HandedSecrets is every value a launch handed its box that a capture of the
// box's session scrubs out by value, and where its seeded logins live.
type HandedSecrets struct {
	// Values is every value the launch handed the box: the run's whole
	// environment, its MCP servers' env and headers, and its setup steps'
	// env and files, written or referenced alike, and the strings its
	// seeded login held.
	Values []string `json:"values"`

	// Logins maps each login file the box was seeded with — its path
	// relative to the box's home ([Spec.CredentialFiles]) — to the file on
	// the engine host it was seeded from, both of which are read again at a
	// capture.
	Logins map[string]string `json:"logins,omitempty"`
}

// MaxHandedBytes bounds the secrets one launch's row keeps, encoded, before
// they are sealed.
//
// SIXTY-FOUR KIBIBYTES. A box is handed a few dozen variables, a handful of
// MCP servers' headers and the files its setup steps write — a token file, an
// `.npmrc`, a deploy key — and a login document of a kibibyte or two: a few
// kibibytes in all, a key file the largest single value. Sixty-four is room for
// a dozen keys beside everything else, and still a small part of a row the
// coordination store reads whole on every write of the run. A launch handed
// more keeps none, and its captures are refused saying so, rather than a row
// grown without bound or a scrub of only some of them.
const MaxHandedBytes = 64 << 10

// ErrHandedUnknown is a capture refused because the secrets its box was
// handed cannot be known: the row keeps none, or a seal this node cannot open.
var ErrHandedUnknown = errors.New("sandbox: the secrets the engine handed the box cannot be read back")

// handedSecrets is what req hands its box, env being the environment the job
// is started with ([withTelemetry]).
func handedSecrets(req LaunchRequest, env map[string]string) HandedSecrets {
	var values []string
	add := func(vs ...string) {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				values = append(values, v)
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		add(env[key])
	}
	for _, name := range slices.Sorted(maps.Keys(req.MCPServers)) {
		server := req.MCPServers[name]
		for _, key := range slices.Sorted(maps.Keys(server.Env)) {
			add(server.Env[key])
		}
		for _, key := range slices.Sorted(maps.Keys(server.Headers)) {
			add(server.Headers[key])
		}
	}
	for _, step := range req.Setup {
		for _, key := range slices.Sorted(maps.Keys(step.Env)) {
			add(step.Env[key])
		}
		for _, file := range slices.Sorted(maps.Keys(step.Files)) {
			add(step.Files[file])
		}
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
			"past the %d a run's record keeps for its session's scrub", len(raw), MaxHandedBytes)
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

// sealHanded is what a launch's row keeps of the secrets it handed its box
// ([PendingRun.Handed]): sealed, or "" where they cannot be — no executor log
// to keep a session for, secrets past [MaxHandedBytes], a keyring that cannot
// seal — which the log says, and which refuses every capture of the job.
func (c *Coordinator) sealHanded(ctx context.Context, req LaunchRequest, env map[string]string) string {
	if !c.keepsSessions() {
		return ""
	}
	raw, err := handedSecrets(req, env).encode()
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
