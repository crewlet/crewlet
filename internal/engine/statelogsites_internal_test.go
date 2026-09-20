package engine

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY REGISTERED DOMAIN IS HANDLED AT EVERY PER-DOMAIN SITE, and the reason
// this is one test rather than four is that the sites fail in four different
// ways and one of them used to fail in no way at all.
//
// A domain is DECLARED in [registeredDomains] and then needs an answer from
// four switches, none of which the declaration can give: a Tier A ceiling
// ([tierACeiling]), a write authority ([stateLog.publisherFor]), a state
// machine ([stateLog.applierFor]) and a barrier encoder
// ([barrierEncoderFor]). Three of them refuse the boot naming the domain. The
// fourth did not: a `nil` encoder is a LEGAL value — the compacted domain
// deliberately has none — so a domain nobody wrote a case for got a reader
// with no read index, and its `linearizable` reads went back to appending no
// barrier while still reporting the level they were asked for. There is no
// error, no log line and no metric for that anywhere.
//
// # Which two are checked here, and why by CALLING them
//
// [tierACeiling] has its own case next door
// (TestEveryRegisteredDomainIsSizedFromTierA) and [stateLog.publisherFor]
// needs a broker and a running applier, so it is covered where the engine
// boots — a registered domain with no arm there fails every boot in this
// package. What is left is the two a test can call with nothing running.
//
// Calling them is the point. A scan of this package's SOURCE for each
// domain's name would go inert the moment somebody renamed a function or
// moved a switch, and would keep reporting a pass over a site that no longer
// exists.
func TestEveryRegisteredDomainIsHandledAtEveryPerDomainSite(t *testing.T) {
	t.Parallel()

	// THE EXPECTED ENCODER IS A STORED VALUE, for the reason
	// domainfeed_internal_test's own group-name table gives: a test that
	// re-derived the mapping from the code under test would agree with
	// whatever that code says, including a copy-paste.
	//
	// AND IT HAS TO BE AN IDENTITY CHECK, because a barrier is
	// deliberately the most domain-AGNOSTIC record any of these logs
	// carries: every one of them renders it as the same kind
	// ([statelog.BarrierKind]) under the same scope sentinel
	// ([statelog.BarrierScope]) with no payload at all. So the wiki's
	// encoder wired into chat's arm produces bytes chat's own
	// [statelog.Domain.Envelope] decodes perfectly — measured — and the
	// round-trip below sees nothing wrong. What it is not is harmless:
	// the record carries the WRITING domain's [RecordVersion], so the
	// day either domain's version moves, every barrier on the other's
	// log is a record its applier holds at a gate — at exactly the
	// position a linearizable read is waiting on.
	//
	// nil is a domain that declares NO encoder, which is a different
	// answer from one nobody wrote a case for; [barrierEncoderFor]'s
	// default arm is what separates them, and the case below it is what
	// proves that arm fires.
	wantEncoder := map[string]barrierEncoder{
		tracker.Domain{}.Name(): tracker.EncodeBarrier,
		pages.Domain{}.Name():   pages.EncodeBarrier,
		chat.Domain{}.Name():    chat.EncodeBarrier,
		search.Domain{}.Name():  nil,
	}

	s := &stateLog{nodeID: "node-under-test"}
	for _, domain := range registeredDomains() {
		want, listed := wantEncoder[domain.Name()]
		if !listed {
			t.Errorf("%s is registered and has no row here: name the encoder "+
				"it renders a barrier with, or nil for a domain whose reads "+
				"make no freshness claim", domain.Name())
			continue
		}
		t.Run(domain.Name(), func(t *testing.T) {
			t.Parallel()

			applier, err := s.applierFor(domain)
			if err != nil {
				t.Errorf("applierFor: %v — its records would be consumed and "+
					"produce no rows on this node", err)
			} else if applier == nil {
				t.Error("applierFor returned no applier and no error")
			}

			encode, err := barrierEncoderFor(domain)
			if err != nil {
				t.Fatalf("barrierEncoderFor: %v", err)
			}
			if !sameFunc(encode, want) {
				t.Fatalf("the barrier encoder is %s, want %s — a sibling's "+
					"encoder here is invisible today and becomes a record "+
					"this domain's applier gates the moment either record "+
					"version moves", funcName(encode), funcName(want))
			}
			if encode == nil {
				// THE HONEST ABSENCE, and it is asserted rather than
				// waved through: a domain whose reads make no
				// freshness claim must also be one that claims no
				// identity, or two nodes would be entitled to
				// disagree about rows a `linearizable` read was
				// never able to certify.
				if domain.ClaimsIdentity() {
					t.Errorf("%s claims byte-identical rows across nodes and "+
						"declares no barrier encoder, so no read of it can "+
						"ever be certified as of a position", domain.Name())
				}
				return
			}

			// AND THE ENCODER WORKS ON THIS DOMAIN'S OWN TERMS. The
			// identity check above says the right function is wired;
			// this says the record it writes is one this domain reads
			// back as a barrier at the generation it was stamped with.
			// A barrier at the wrong generation is one every applier
			// reads as safely stale, which is a linearizable read
			// waiting on a position nothing will ever confirm.
			payload, err := encode(statelog.Envelope{
				Kind: statelog.BarrierKind, Gen: 7,
			})
			if err != nil {
				t.Fatalf("encode a barrier: %v", err)
			}
			env, err := domain.Envelope(payload)
			if err != nil {
				t.Fatalf("%s cannot decode the barrier its own encoder wrote: "+
					"%v", domain.Name(), err)
			}
			if env.Kind != statelog.BarrierKind {
				t.Errorf("the barrier decoded as kind %q, want %q", env.Kind,
					statelog.BarrierKind)
			}
			if env.Gen != 7 {
				t.Errorf("the barrier carried generation %d, want the 7 it "+
					"was encoded at", env.Gen)
			}
			if env.V != domain.RecordVersion() {
				t.Errorf("the barrier is at record version %d and %s applies "+
					"version %d — a record above this build's version is one "+
					"the applier HOLDS, at the position a linearizable read "+
					"is waiting on", env.V, domain.Name(), domain.RecordVersion())
			}
		})
	}
}

// AND A DOMAIN NOBODY WROTE A CASE FOR IS REFUSED, LOUDLY.
//
// This is the half that makes the case above able to fail for the right
// reason. Without a default arm [barrierEncoderFor] answers `nil, nil` for an
// unhandled domain, which is indistinguishable from the compacted domain's
// deliberate absence — so the register could grow a fifth domain, every test
// above would pass, and that domain's reads would quietly stop meaning what
// they say.
func TestAnUnregisteredDomainIsRefusedABarrierEncoder(t *testing.T) {
	t.Parallel()
	stranger := unhandledDomain{Domain: pages.Domain{}}
	encode, err := barrierEncoderFor(stranger)
	if err == nil {
		t.Fatalf("barrierEncoderFor(%q) returned an encoder=%v and no error — "+
			"a domain no case names must not be handed the same nil the "+
			"compacted domain declares on purpose", stranger.Name(), encode != nil)
	}
	if !strings.Contains(err.Error(), stranger.Name()) {
		t.Errorf("the refusal is %q and does not name the domain — a boot "+
			"refused over one of four switches has to say which domain to go "+
			"and write a case for", err)
	}
}

// barrierEncoder is the shape every one of them has, which is exactly why one
// cannot be told from another without comparing the function itself.
type barrierEncoder = func(statelog.Envelope) ([]byte, error)

// sameFunc reports whether got is the same function as want.
//
// BY CODE POINTER, which is the only thing that distinguishes two functions of
// one signature: Go refuses `==` on func values, and every barrier encoder in
// this tree takes and returns the same types. All of them are plain
// package-level functions, so the pointer is the entry point and is stable.
func sameFunc(got, want barrierEncoder) bool {
	if got == nil || want == nil {
		return (got == nil) == (want == nil)
	}
	return reflect.ValueOf(got).Pointer() == reflect.ValueOf(want).Pointer()
}

// funcName is a function value's own name, or "none" for nil.
func funcName(f barrierEncoder) string {
	if f == nil {
		return "none"
	}
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		return "an unnamed function"
	}
	return fn.Name()
}

// unhandledDomain is a registered domain's declaration under a name no switch
// in this package knows.
//
// EMBEDDED RATHER THAN HAND-WRITTEN: [statelog.Domain] is twelve methods, and
// a stub implementing all twelve would be twelve more things to keep correct
// for a case that only ever reads Name.
type unhandledDomain struct{ statelog.Domain }

func (unhandledDomain) Name() string { return "a-domain-nobody-wrote-a-case-for" }

// NATIVE CHAT CONTRIBUTES BOTH HALVES OF THE INBOUND EDGE, and each half is
// silent when it is missing.
//
// A wake is only ever delivered to the source whose PARSER registered it, and
// only ever rendered by the PROMPT whose Source matches. Drop the parser arm
// and a company's messages commit, feed and wake nobody — every seat simply
// never hears about its own rooms. Drop the prompt arm, or write it as a zero
// `chat.Prompt{}`, and [notify.NewPrompts] skips it on an empty Source: every
// chat wake then renders through the generic fallback, which names none of the
// tools a turn discharges its obligation with. Neither failure logs anything.
//
// The arm is reached on a runtime field rather than a booted engine, because
// what is under test is the WIRING and not the store: [chat.NewStore]'s own
// refusals are its package's.
func TestNativeChatContributesBothAParserAndAPrompt(t *testing.T) {
	t.Parallel()
	e := &Engine{native: &native{chat: &chat.Store{}}}
	parsers, prompts := e.nativeParsers(&Company{})

	var sources []string
	for _, p := range parsers {
		sources = append(sources, p.Source())
	}
	if len(parsers) != 1 || sources[0] != chat.Source {
		t.Fatalf("a native-chat node registered parsers for %v, want exactly "+
			"[%q] — a company whose chat has no parser commits every message "+
			"and wakes nobody at all", sources, chat.Source)
	}

	sources = nil
	for _, p := range prompts {
		sources = append(sources, p.Source())
	}
	if len(prompts) != 1 || sources[0] != chat.Source {
		t.Fatalf("a native-chat node registered prompts for %v, want exactly "+
			"[%q] — a wake whose source no prompt answers for renders through "+
			"the generic fallback, which names none of the tools a turn "+
			"discharges its obligation with", sources, chat.Source)
	}

	// AND IT IS THE CONSTRUCTED PROMPT rather than a `chat.Prompt{}`. The
	// source would survive that — the package declares Source on the type
	// for exactly this reason — but the EMBEDDED value would not: a zero
	// [notify.ChatPrompt.Address] reads a direct conversation as an
	// ordinary room, so a person's consecutive messages in a DM land in as
	// many turns as they typed and nothing anywhere reports it.
	built, isChats := prompts[0].(chat.Prompt)
	if !isChats {
		t.Fatalf("the chat prompt is a %T, which is not this package's", prompts[0])
	}
	if len(built.Address.DirectKinds) != len(chat.AddressRule().DirectKinds) {
		t.Errorf("the registered prompt addresses %v as direct and the package "+
			"says %v — the zero rule reads a DM as a room",
			built.Address.DirectKinds, chat.AddressRule().DirectKinds)
	}
}
