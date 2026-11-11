package estate

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/secrets"
)

// accessSealer is a sealer over a keyring of one key.
func accessSealer(t *testing.T) *accessdomain.Sealer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": key}})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := accessdomain.NewSealer(cipher)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

// accessRouter is a router that records what it asks of a data node that
// serves nothing.
func accessRouter(t *testing.T) (*Router, *recordingAsker) {
	t.Helper()
	asker := &recordingAsker{answer: reply{Node: "data-a", Unserved: unservedNoBackend,
		Detail: "data-a runs no native backend for it"}}
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: asker,
		Placement: &fakePlacement{nodes: []string{"data-a"}}, Git: &fakePlacement{}, Session: NewSession(),
		Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	return r, asker
}

// THE FACADES ASK EXACTLY THE ACCESS LOG'S OPERATIONS, each method one: every
// access read through the reader facade and every access write through the
// writer facade, and nothing else through either.
//
// Mutation: point AccessWrites.Issued at opAccessTell and the writer's
// operations lack access.issued.
func TestTheAccessFacadesAskExactlyTheAccessOperations(t *testing.T) {
	t.Parallel()
	r, asker := accessRouter(t)
	for _, tc := range []struct {
		facade any
		want   func(*opSpec) bool
	}{
		{r.AccessReads(), func(s *opSpec) bool { return s.domain == accessDomain && s.class == opRead }},
		{r.AccessWrites(accessSealer(t)), func(s *opSpec) bool {
			return s.domain == accessDomain && s.class != opRead
		}},
	} {
		v := reflect.ValueOf(tc.facade)
		got := map[string]bool{}
		for i := range v.NumMethod() {
			method, m := v.Type().Method(i).Name, v.Method(i)
			in := make([]reflect.Value, m.Type().NumIn())
			in[0] = reflect.ValueOf(t.Context())
			for j := 1; j < len(in); j++ {
				in[j] = reflect.Zero(m.Type().In(j))
			}
			before := len(asker.requests())
			m.Call(in)
			sent := asker.requests()[before:]
			if len(sent) != 1 {
				t.Errorf("%s.%s asked %d operations, want one", v.Type().Name(), method, len(sent))
				continue
			}
			got[opened(sent[0].payload).Op] = true
		}
		want := map[string]bool{}
		for name, spec := range registry {
			if tc.want(spec) {
				want[name] = true
			}
		}
		if len(want) == 0 || !maps.Equal(got, want) {
			t.Errorf("%s asks %v, want exactly %v", v.Type().Name(),
				slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
	}
}

// A COMMAND CROSSES THE ROUTER SEALED: the request a seat's node sends a data
// node carries no command text, only its seal, which opens under the fleet
// keyring as that command's record.
//
// Mutation: send the command in the clear in AccessWrites.Ran and its text is
// in the request.
func TestACommandCrossesTheRouterSealed(t *testing.T) {
	t.Parallel()
	r, asker := accessRouter(t)
	sealer := accessSealer(t)
	const text = "sudo systemctl restart billing-worker-7731"
	id := uuid.NewString()
	_, _ = r.AccessWrites(sealer).Ran(t.Context(), accessdomain.Call{}, accessdomain.NewCommand{
		ID: id, Handle: "sre", Host: "api-1", User: "crewlet-ops", Level: accessdomain.LevelOperate,
		Command: text,
	})
	sent := asker.requests()
	if len(sent) != 1 {
		t.Fatalf("Ran asked %d operations, want one", len(sent))
	}
	req := opened(sent[0].payload)
	if req.Op != "access.ran" || bytes.Contains(req.Args, []byte("billing-worker-7731")) {
		t.Fatalf("the request asks %s and carries %s, want access.ran with no command text", req.Op, req.Args)
	}
	var args accessRanArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		t.Fatal(err)
	}
	if args.Command.Command != "" {
		t.Errorf("the request names the command %q beside its seal", args.Command.Command)
	}
	opened, err := sealer.Open(args.Sealed.Sealed, accessdomain.AAD("sre", id))
	if err != nil || opened != text || args.Sealed.Key != "k1" {
		t.Errorf("the seal opens to %q under %q, %v; want the command under k1", opened, args.Sealed.Key, err)
	}
	if _, err := r.AccessWrites(nil).Ran(t.Context(), accessdomain.Call{},
		accessdomain.NewCommand{ID: id, Handle: "sre", Command: text}); err == nil {
		t.Error("a router with no keyring sent a command")
	}
}

// A VERDICT CROSSES THE ROUTER SEALED (ADR-0125): the request a delegate's node
// sends a data node carries neither what was decided nor the delegate's words,
// only their seals, which open under the fleet keyring as that verdict's.
//
// Mutation: send the subject or the reason in the clear in
// AccessWrites.Verdict and its text is in the request.
func TestAVerdictCrossesTheRouterSealed(t *testing.T) {
	t.Parallel()
	r, asker := accessRouter(t)
	sealer := accessSealer(t)
	const subject, reason = "sudo systemctl restart billing-worker-7731", "routine for ticket-4471"
	_, _ = r.AccessWrites(sealer).Verdict(t.Context(), accessdomain.Call{}, accessdomain.NewVerdict{
		Request: uuid.NewString(), Kind: delegated.KindSSHOperate, Rule: 1, Handle: "sre", By: "security",
		ByKind: accessdomain.VerdictByAgent, Through: accessdomain.VerdictThroughDelegation,
		Asked: delegated.Allow, Confidence: delegated.High, Verdict: delegated.Allow, Final: true,
		Digest: strings.Repeat("ab", 32), Host: "api-1", Subject: subject, Reason: reason,
		Findings: []string{"restarts ticket-4471's worker"},
	})
	sent := asker.requests()
	if len(sent) != 1 {
		t.Fatalf("Verdict asked %d operations, want one", len(sent))
	}
	req := opened(sent[0].payload)
	if req.Op != "access.verdict" || bytes.Contains(req.Args, []byte("billing-worker-7731")) ||
		bytes.Contains(req.Args, []byte("ticket-4471")) {
		t.Fatalf("the request asks %s and carries %s, want access.verdict with no text in the clear", req.Op, req.Args)
	}
	if _, err := r.AccessWrites(nil).Verdict(t.Context(), accessdomain.Call{},
		accessdomain.NewVerdict{Request: uuid.NewString(), Handle: "sre", Subject: subject}); err == nil {
		t.Error("a verdict was recorded with no keyring to seal it under")
	}
}
