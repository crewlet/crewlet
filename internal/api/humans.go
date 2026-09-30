package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE SURFACES A PERSON REACHES, built over the engine running beside them:
// the way in, the identity directory, the human write surface, and the two
// credential arms the guard resolves a browser's cookie and a machine token
// through.
//
// # Here, beside the options they fill, and not in the command that serves them
//
// For [engineRuntime]'s reason, with its failure measured. They were built in
// cmd/crewlet, which is package main and reachable from no other package, so
// the end-to-end suite's own wiring could not call them and mounted none of
// them: every fixture there presented a Tier A bearer to a node serving no
// /auth, no /iam and no /work. A fleet whose sessions did not cross between
// its members — a cookie minted on one node refused on the next, a revocation
// one node never heard — left the fleet suite green, because nothing in it had
// ever signed in. One constructor that `crewlet run` and the suite both call is
// what makes the suite's sign-in the one a deployment serves; a copy in the
// suite would be a harness agreeing with itself.

// HumanSurfaces are the surfaces a node serves a person, and a person's
// credentials, through. Built by [NewHumanSurfaces] and handed to an API's
// options by [HumanSurfaces.Mount].
type HumanSurfaces struct {
	// SignIn serves /auth — how a person BECOMES a principal. Every node
	// serves it, one that has met no company included: that is the node
	// its company's first person is invited to.
	//
	// HELD BY WHOEVER SERVES IT as well as mounted, for
	// [authapi.Service.Stop]: the work a sign-in runs after its answer — a
	// stale password's rewrite — is the surface's own to stop, after the
	// listener closes and before the engine it writes to does.
	SignIn *authapi.Service

	// Sessions is the other end of the cookie SignIn mints, built from the
	// same signer.
	Sessions *auth.Sessions

	// Tokens resolves the machine tokens /iam/credentials mints — `crewlet
	// iam token`'s value, presented as CREWLET_API_TOKEN.
	Tokens *auth.Tokens

	// Directory serves /iam, and Work the human write surface over the
	// native tracker and knowledge base — which reads the halves it serves
	// per request, since a node meets them at its first company and may
	// meet that long after this surface was built.
	Directory guardedMounter
	Work      guardedMounter
}

// NewHumanSurfaces builds a node's human surfaces over its engine and the Tier
// A it runs under.
//
// EVERY NODE SERVES ALL OF THEM, with a company or without one. The identity
// estate is the engine's CORE, running from boot on every node, so a node
// nobody has configured yet still signs people in, holds the directory its
// first person is invited through, and serves the write surface — whose
// routes answer `503 no_active_revision` until the first company brings the
// tracker and the knowledge base up, and are served from then on with no
// restart. They used to be ABSENT on such a node, each logged, so the node a
// company is bootstrapped on was the one node nobody could sign in to until
// it was restarted.
func NewHumanSurfaces(boot *config.Bootstrap, e *engine.Engine) (HumanSurfaces, error) {
	signIn, sessions, err := signInSurface(boot, e)
	if err != nil {
		return HumanSurfaces{}, err
	}
	// AND THE THIRD CREDENTIAL, a machine token the directory minted.
	tokens, err := auth.NewTokens(auth.TokensDeps{
		Directory: e, Chart: engine.SeatViewOf(e),
	})
	if err != nil {
		return HumanSurfaces{}, fmt.Errorf("api: the machine-token arm: %w", err)
	}
	// AND THE DIRECTORY.
	directory, err := directorySurface(boot, e)
	if err != nil {
		return HumanSurfaces{}, err
	}
	// AND THE WRITE SURFACE, over the halves each request finds.
	work, err := workSurface(e)
	if err != nil {
		return HumanSurfaces{}, err
	}
	return HumanSurfaces{
		SignIn: signIn, Sessions: sessions, Tokens: tokens,
		Directory: directory, Work: work,
	}, nil
}

// Mount hands the surfaces to an API's options.
//
// THE ONE PLACE A NIL SURFACE BECOMES A NIL INTERFACE, and it is a method so
// that no caller does the conversion for itself. A surface held as a nil *T is
// a NON-NIL interface once it reaches [Options], so [New]'s own "is there one"
// check passes and mounts every route over a nil service — which answers each
// request with a nil dereference instead of the 404 an absent surface is
// documented to be. [NewHumanSurfaces] builds every one on an engine [engine.New]
// made; a zero HumanSurfaces — a suite's — is what this keeps honest.
func (h HumanSurfaces) Mount(o *Options) {
	if h.SignIn != nil {
		o.Auth = h.SignIn
	}
	o.Sessions, o.Tokens = h.Sessions, h.Tokens
	o.IAM, o.Work = h.Directory, h.Work
}

// signInSurface builds /auth and the session arm beside it.
//
// ON EVERY NODE. The identity estate is the engine's core, so a node that has
// met no company holds its directory too — and is exactly where the company's
// first person is invited and signs in. An engine with no identity estate is
// one [engine.New] did not build, which is a wiring mistake to refuse by
// name rather than a posture to serve around.
//
// A keyring that cannot sign for the fleet USED TO BE a narrower posture too,
// and it is not any more: Tier A refuses a file without a usable keyring and
// the engine refuses to start without one, so a session signer this function
// cannot build is a fault it returns rather than a node it quietly narrows.
func signInSurface(boot *config.Bootstrap, e *engine.Engine) (
	*authapi.Service, *auth.Sessions, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		return nil, nil, errNoIdentityEstate
	}
	signer, err := session.New(session.Options{
		Material: boot.Secrets.TokenMaterial(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session signer: %w", err)
	}
	// THIS NODE'S OWN CURVE, and nobody else's: a guessing run a load
	// balancer rotates across the fleet meets each node's separately, which
	// is the residual internal/iam/credential's throttle states and bounds.
	throttle := credential.NewThrottle(credential.ThrottleDeps{})
	// THE SEALER, checked here for the directory's reason: a typed nil in
	// the surface's interface would pass its own refusal.
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil, nil, errors.New("api: the sign-in surface: this node " +
			"holds no keyring to seal a second factor with")
	}
	surface, err := authapi.New(authapi.Options{
		Bootstrap: boot,
		Directory: reader,
		// THE NODE'S OWN WRITER, which acts as the deployment. What the
		// routes do with it is create people and open sessions, both of
		// which are the deployment's to do on somebody's behalf — a
		// person cannot author their own enrolment, because they do not
		// exist until it lands.
		Writer:   writer,
		Signer:   signer,
		Hasher:   credential.NewHasher(credential.Default(), credential.VerifyCap()),
		Throttle: throttle,
		Blinder:  e.PersonBlinder(),
		Sealer:   sealer,
		Sessions: reader,
		// THE SAME PURE FUNCTION THE GUARD USES over the same Tier A,
		// which is one PARSER rather than one instance — see
		// [auth.Clients].
		Clients: auth.NewClients(boot),
		// THE NODE'S ONE AUDIT TRAIL, which the guard and the directory
		// hand what they saw to as well: a failed sign-in and a refused
		// bearer fold into one row per client per minute only because
		// both reach the same tally.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the sign-in surface: %w", err)
	}
	// AND THE OTHER HALF, built from the SAME signer. A cookie minted
	// under one key and validated against another is a sign-in that
	// appears to work and then does not stick — and it would do so only
	// on the requests that landed on a node whose signer was built
	// separately, which is the shape nobody reproduces.
	sessions, err := auth.NewSessions(auth.SessionsDeps{
		Signer:    signer,
		Directory: reader,
		// THE SAME READER'S APPLIER, which a write presenting a session
		// this node has not applied yet waits on — the sign-in above
		// answers before it does.
		Applier: reader,
		// THE CHART VIEW, and the ZERO VALUE on a node with no chart
		// domain — never nil, which internal/iam/session reads as the
		// seatless arm. See [engine.SeatViewOf].
		Chart:    engine.SeatViewOf(e),
		External: boot.API.ExternalBase(),
		// The same trail, whose once-per-lineage claim is what keeps a
		// cookie presented past its deadline announcing that ending once.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session arm: %w", err)
	}
	return surface, sessions, nil
}

// directorySurface builds /iam, on every node for [signInSurface]'s reason.
func directorySurface(boot *config.Bootstrap, e *engine.Engine) (guardedMounter, error) {
	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		return nil, errNoIdentityEstate
	}
	// THE KEYRING'S OPENER, checked here rather than handed in as a typed
	// nil: an interface holding a nil pointer is not nil, so the surface's
	// own refusal would never see it and the first name it opened would
	// panic.
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil, errors.New("api: the identity directory: this node holds " +
			"no keyring to open anybody's name with")
	}
	surface, err := iamapi.New(iamapi.Options{
		Directory: reader,
		// ONE WRITER PER CALLER. The node's own writer acts as the
		// DEPLOYMENT, which is right for a bootstrap and wrong for
		// everything here: a directory whose author field is the node
		// is not an audit trail. The party is the caller's principal,
		// whole — its name, the credential beside it, its grants and its
		// id, which a person's own mint is decided on.
		Authority: func(principal iam.Principal) iamapi.Writer {
			return writer.As(principal)
		},
		Opener:       sealer,
		ExternalBase: boot.API.ExternalBase(),
		// THIS NODE'S OWN CEILING, which the report compares a person's
		// declared grants against: it is applied at decision time and
		// never written, so a fleet mid-rollout legally disagrees and
		// nothing else would say so.
		Ceiling:  boot.API.Auth.MaxGrants,
		Bindings: danglingBindings(e),
		// What an administrator did — a token minted or revoked, a
		// session ended, a person removed — on the node's audit feed.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, fmt.Errorf("api: the identity directory: %w", err)
	}
	return surface, nil
}

// danglingBindings is the dangling-binding arm of the directory report, or nil
// where this node cannot run it.
//
// THE ENGINE'S RULE, not one written here: [engine.Engine.DanglingBinding] is
// the request path's own seat table applied to a person's row, and the
// `iam_binding_dangling` alarm asks the same function — so the report and the
// alarm cannot disagree about which binding dangles. The seam it replaced
// asked only whether the chart held a row by that handle, which is how a
// person bound to an AGENT seat went unreported while every request they made
// was refused.
//
// NIL ON AN ENGINE WITH NO CHART READER — one [engine.New] did not build —
// which is the same third value a nil [Options.SeatHeld] is: there are no rows
// to ask, so the arm is skipped rather than asked.
func danglingBindings(e *engine.Engine) iamapi.Bindings {
	if e.Chart() == nil {
		return nil
	}
	return func(ctx context.Context, row iamdomain.PersonRow) (bool, string, error) {
		residue, dangling, err := e.DanglingBinding(ctx, row)
		return dangling, residue.Detail, err
	}
}

// errNoIdentityEstate is a human surface asked of an engine that holds no
// identity estate — one [engine.New] did not build, since every engine it
// builds runs the estate from boot.
var errNoIdentityEstate = errors.New("api: the engine runs no identity estate, " +
	"so there is nobody to sign in or enrol — an engine built by engine.New " +
	"always does, from boot, whether or not it has a company")

// workSurface builds the human write surface over the halves each request
// finds.
//
// FROM THE SAME DEPS the operator's assistant is served from — see
// [NativeToolDeps] — and deciding on the same chart, so a route and the tool
// behind it answer one question one way. What it adds is the two writers the
// tools never reach: the tracker's, bound to the caller, for a rank move, a
// comment edit and the purge, and the knowledge base's store for its rename
// and its three destructive verbs.
//
// PER REQUEST, never captured here: the halves come up with the node's first
// company, which a node that booted with none meets at an apply after this
// surface is serving. See [workapi.Options.Halves].
func workSurface(e *engine.Engine) (guardedMounter, error) {
	surface, err := workapi.New(workapi.Options{
		Halves: func() (workapi.Halves, bool) { return nativeHalves(e) },
		Chart:  engine.ChartAuthorityOf(e),
	})
	if err != nil {
		return nil, fmt.Errorf("api: the work surface: %w", err)
	}
	return surface, nil
}

// nativeHalves are the tracker and knowledge-base halves this engine serves
// now, and false where it has not been handed a company yet.
//
// [engine.Engine.NativeStarted] FIRST, and the halves after: it is monotonic,
// so a half read as absent after it said "started" is one this company does
// not run rather than one not published yet — see native.go.
func nativeHalves(e *engine.Engine) (workapi.Halves, bool) {
	if !e.NativeStarted() {
		return workapi.Halves{}, false
	}
	work, kb := NativeToolDeps(e)
	halves := workapi.Halves{Work: work, Pages: kb}
	if writer := e.TrackerWriter(); writer != nil {
		halves.Tracker = func(actor builtin.Actor) workapi.TrackerWriter {
			return personWriter(writer, actor)
		}
	}
	// THE CONVERSION IS THE POINT: a typed nil *pages.Store inside the
	// interface would pass the surface's own check and panic on the first
	// press.
	if store := e.PagesStore(); store != nil {
		halves.PageStore = store
	}
	return halves, true
}

// personWriter is the node's tracker writer acting as one person-facing
// actor: the author [builtin.PrincipalActor] made of the request's principal,
// of its kind, with the credential it acted through.
//
// ONE HELPER FOR EVERY SEAM a person's surface writes the tracker through —
// the nine tool seams below and the work surface's own writer — because
// turning a tool-layer actor into a writer is a single rule, and ten
// hand-copied spellings of it are ten chances for one seam to record an
// identity the others do not.
func personWriter(w *tracker.Writer, actor builtin.Actor) *tracker.Writer {
	return w.As(actor.Handle, actor.Kind, tracker.Provenance{OperatorID: actor.OperatorID})
}

// NativeToolDeps are the deps the builtin tools are built from for a surface
// whose caller is a PERSON rather than a seat: the operator's assistant over
// MCP, and the HTTP write surface.
//
// ONE CONSTRUCTOR FOR BOTH, because the two serve the same tools and a field
// wired on one and forgotten on the other is a tool that behaves differently
// depending on where it was called from — which is the drift each of the
// comments below records having happened once already. Actor and Authorize are
// left for each surface to set: the first carries a per-request key on one of
// them, and the second is decided where the surface is built.
//
// The DEFAULTS are deliberately absent. A seat files into its unit's project
// when it names none, because a seat HAS a unit; a person does not, so the
// argument is required and the tool refuses naming it rather than guessing a
// project on somebody's behalf.
func NativeToolDeps(e *engine.Engine) (builtin.WorkDeps, builtin.PageDeps) {
	var work builtin.WorkDeps
	var kb builtin.PageDeps
	if reader, writer := e.Tracker(), e.TrackerWriter(); reader != nil && writer != nil {
		work = builtin.WorkDeps{
			Reader: reader,
			// THE OPERATOR'S OWN CREDENTIAL IS THE PARTY, and it comes
			// from the request's context rather than from the call: a
			// tracker whose author field is chosen by the writer is not
			// an audit trail, and there is deliberately no way to name a
			// seat to act as.
			Writer: func(actor builtin.Actor) builtin.WorkWriter {
				return personWriter(writer, actor)
			},
			// AND THE TWO SEQUENCES, which this surface went
			// without — so an operator's assistant was refused
			// `waiting_on` and `blocking` by name on a tool whose
			// own description offers them, and would not have been
			// served the fold at all. Both need the replicated
			// estate, which this writer has; nothing else about
			// them differs from a seat's.
			Dependencies: func(actor builtin.Actor) builtin.WorkDepender {
				return personWriter(writer, actor)
			},
			Merges: func(actor builtin.Actor) builtin.WorkMerger {
				return personWriter(writer, actor)
			},
			// AND THE CROSS-PROJECT MOVE, the third sequence: a subtree
			// walked record by record onto another project's keys, which
			// needs the replicated estate for the walk exactly as the
			// two above do. Without it `move_work_item` is not
			// registered here at all, and the only way to move a
			// person's item was to ask a seat to.
			Moves: func(actor builtin.Actor) builtin.WorkMover {
				return personWriter(writer, actor)
			},
			// AND THE RANKED SEARCH. It reads, so it takes no actor —
			// the corpus is the same for everybody and there is nothing
			// to attribute — and without it the operator catalogue
			// listed a verb this surface could never register.
			Search: engine.WorkSearcher(e),
			// THE SAVED-VIEW WRITER, which only this surface has: a
			// view is furniture a person arranges, and no seat is
			// given the tools that reach it.
			ViewWriter: func(actor builtin.Actor) builtin.ViewWriter {
				return personWriter(writer, actor)
			},
			// AND THE CATALOGUE WRITER: the company's own vocabulary is
			// a person's to set, never a seat's to widen so its own
			// create succeeds.
			CatalogueWriter: func(actor builtin.Actor) builtin.CatalogueWriter {
				return personWriter(writer, actor)
			},
			// AND THE PERSON WRITER. Who may write what is the
			// tracker's own rule; what this surface supplies is the
			// identity it is judged against.
			PersonWriter: func(actor builtin.Actor) builtin.PersonWriter {
				return personWriter(writer, actor)
			},
			// AND THE INBOX READ. It takes no actor for the reason
			// Search takes none — it reads, and whose inbox is an
			// argument rather than an identity — and it is this
			// surface's alone beside the person writer, because a
			// seat has a mailbox rather than an inbox.
			Inbox: reader,
			// AND THE TRASH. A removal takes an item off every board in
			// the company and a restore puts it back at any age; neither
			// destroys anything, which is what separates both from the
			// purge the CLI guards with a typed confirmation. No seat
			// holds either — see internal/agent/builtin/worktrash.go.
			TrashWriter: func(actor builtin.Actor) builtin.TrashWriter {
				return personWriter(writer, actor)
			},
			// AND A PROJECT'S OWN SETTINGS. Unlike the five above,
			// this one is on every surface — declaring a tag is open
			// to every seat — and what an operator adds here is the
			// credential the archive facet asks for.
			ProjectWriter: func(actor builtin.Actor) builtin.ProjectWriter {
				return personWriter(writer, actor)
			},
			// WHOSE RECORD A LOGIN NAMES, which every person verb
			// resolves its name through: a login is never a seat, and
			// read literally a bound person's named a record nothing of
			// theirs is kept under.
			Holders: e,
			// THE ROSTER, so an operator's assistant is refused a
			// handle nobody has rather than silently filing work for
			// one — the same check every seat's tools make.
			Seats: func() []colleague.Seat {
				c := e.Company()
				if c == nil {
					return nil
				}
				return builtin.Corpus(c.Org, e.WithheldContacts())
			},
			// AND THE THREE CHART SEAMS THE SEAT SURFACE HAS AND THIS
			// ONE WENT WITHOUT. Their absence was invisible and not
			// harmless: with no Leads, an operator filing an unassigned
			// task woke nobody at all — the lead fallback is what
			// catches exactly that task — and with no Units every
			// project this surface listed read as belonging to no team.
			Leads:          engine.LiveLeads(e),
			Units:          engine.LiveUnits(e),
			DefaultProject: func(string) string { return "" },
			// THE MENTION RESOLVER, which this surface went without: a
			// comment's @-mention is turned into a wake by the tracker's
			// recipients only when the writer resolved it, so an
			// operator writing "@alice can you take this" reached her
			// watchers and never her — while the tool's own description,
			// which their assistant reads, promised it would.
			Mentions: engine.LiveMentions(e),
			Await:    e.WaitCommitted,
		}
	}
	if reader, writer := e.Pages(), e.PagesStore(); reader != nil && writer != nil {
		kb = builtin.PageDeps{
			Reader: reader, Writer: writer,
			Mentions: engine.LiveMentions(e),
			// THE SKILLS CONTAINER, so a person's write into it is asked
			// for the grant a tool skill takes rather than admitted on
			// knowledge:write alone — the store exempts every person,
			// because capability is not its question.
			SkillsContainer: engine.LiveSkillsContainer(e),
			Await:           e.WaitCommitted,
		}
	}
	return work, kb
}
