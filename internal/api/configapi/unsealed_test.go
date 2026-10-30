package configapi_test

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// An ACTIVE REVISION STORED WITHOUT A SEAL is refused by every reader, and the
// refusal names a whole document sent with PUT /config, or `crewlet config
// import`, as what replaces it. These cases hold the online half of that
// promise: the PUT it names lands, reading nothing from the revision it
// replaces, and nothing else does.

// unsealedOrigin is where an unsealed active revision came from: this node's
// own copy, or a body the fleet's pointer carries that this node never kept.
type unsealedOrigin struct {
	name string
	code httpjson.Code
	// seed makes doc, unsealed, the active revision of s, answering its id.
	seed func(t *testing.T, s *surface, doc string) string
}

// unsealedOrigins are both: the two codes, and the two places a replacement's
// parent is or is not kept.
var unsealedOrigins = []unsealedOrigin{
	{"this node's own copy", httpjson.CodeUnsealedRevision,
		func(t *testing.T, s *surface, doc string) string { return s.seedUnsealed(t, doc) }},
	{"the fleet's pointer", httpjson.CodeUnsealedFleetRevision,
		func(t *testing.T, s *surface, doc string) string { return s.unsealedOnThePointer(t, doc) }},
}

// unsealedOnThePointer points the fleet at doc stored without a seal, as a
// body from outside the fleet arrives: this node keeps no copy of it.
func (s *surface) unsealedOnThePointer(t *testing.T, doc string) string {
	t.Helper()
	return s.onThePointer(t, "plaintext-on-the-pointer", companyJSON(t, doc))
}

// onThePointer points the fleet at payload, byte for byte, under id, as a
// body this node never stored arrives: it keeps no copy of it.
func (s *surface) onThePointer(t *testing.T, id string, payload []byte) string {
	t.Helper()
	if _, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: "on the pointer", Payload: payload, At: pinned,
		Origin: coord.RevisionOrigin{Author: "somebody", AuthorKind: iam.ActorOperator,
			Source: "broker", CreatedAt: pinned},
	}); err != nil {
		t.Fatalf("put the body on the pointer: %v", err)
	}
	return id
}

// pointer is the revision the fleet's activation pointer names.
func (s *surface) pointer(t *testing.T) string {
	t.Helper()
	target, found, err := s.plane.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("the activation pointer: %v (found=%v)", err, found)
	}
	return target.RevisionID
}

// restoredDoc is companyDoc as somebody sends it back from their own copy to
// replace an unsealed revision: whole, every credential a real value.
var restoredDoc = strings.Replace(companyDoc, "name: Acme", "name: Acme Restored", 1)

// A WHOLE DOCUMENT REPLACES AN UNSEALED ACTIVE REVISION, as the offline import
// does, from this node's own copy and from the fleet's pointer alike.
//
// The refusal of an unsealed revision names PUT /config, and the write it
// named was refused in the same words, because a write opened the active
// revision before anything else: the remedy was a loop. A whole document reads
// nothing from the revision it replaces, so it lands, SEALED, built on it: its
// If-Match is judged against it as against any active revision, and the
// activation is conditioned on it. A DRY RUN of it is answered and stores
// nothing, and an IMPORT lands as an import. Its row names the unsealed
// revision as its parent where this node holds it, and KEEPS NOTHING where it
// arrived only on the pointer: the reconciler refuses to keep a body from
// outside the fleet, and a write replacing one is no reason to.
//
// Mutation: drop the unsealed arm from prepare and every write is the 409
// again; keep the fleet's body as the parent and it enters this node's
// history.
func TestAWholeDocumentReplacesAnUnsealedRevision(t *testing.T) {
	t.Parallel()
	for _, origin := range unsealedOrigins {
		for _, query := range []string{"", "?import=true"} {
			t.Run(origin.name+" "+cmp.Or(query, "PUT"), func(t *testing.T) {
				t.Parallel()
				s := newSurface(t)
				plain := origin.seed(t, s, companyDoc)
				before, err := s.configs.List(t.Context(), 100, 0)
				if err != nil {
					t.Fatal(err)
				}

				// A DRY RUN, built on the unsealed revision, stores nothing.
				dry := s.do(t, http.MethodPut, "/config"+query+sep(query)+"dry_run=true",
					restoredDoc, nil)
				if body := decode(t, dry); dry.Code != http.StatusOK ||
					body["valid"] != true || body["base_revision_id"] != plain {
					t.Fatalf("a dry run over %s = %d %v, want 200 valid on %s", origin.name,
						dry.Code, body, plain)
				}
				after, err := s.configs.List(t.Context(), 100, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(after) != len(before) || s.pointer(t) != plain {
					t.Fatalf("a dry run stored %d revisions (from %d) or moved the "+
						"pointer to %s", len(after), len(before), s.pointer(t))
				}

				// THE PRECONDITIONS ARE JUDGED AGAINST IT.
				for _, h := range []struct {
					header, value string
					code          httpjson.Code
				}{
					{"If-Match", `"another-revision"`, httpjson.CodeRevisionAdvanced},
					{"If-None-Match", "*", httpjson.CodeAlreadyConfigured},
				} {
					res := s.do(t, http.MethodPut, "/config"+query, restoredDoc,
						map[string]string{"X-Summary": "restore", h.header: h.value})
					if body := decode(t, res); res.Code != http.StatusPreconditionFailed ||
						body["error"] != string(h.code) {
						t.Errorf("%s: %s over %s = %d %v, want 412 %s", h.header, h.value,
							origin.name, res.Code, body, h.code)
					}
				}

				res := s.do(t, http.MethodPut, "/config"+query, restoredDoc,
					map[string]string{"X-Summary": "restore from my own copy",
						"If-Match": `"` + plain + `"`})
				if res.Code != http.StatusCreated {
					t.Fatalf("PUT /config%s over %s = %d, want 201: %s", query,
						origin.name, res.Code, res.Body)
				}
				id, _ := decode(t, res)["revision_id"].(string)
				if s.pointer(t) != id {
					t.Errorf("the pointer names %s, want the replacement %s", s.pointer(t), id)
				}
				stored, held, err := s.configs.Get(t.Context(), id)
				if err != nil || !held {
					t.Fatalf("the replacement is not stored here: %v (held=%v)", err, held)
				}
				if !secrets.Sealed(stored.Payload) {
					t.Error("the replacement was stored without a seal")
				}
				wantSource := "api"
				if query != "" {
					wantSource = "file"
				}
				if stored.Source != wantSource {
					t.Errorf("the replacement is recorded from %q, want %s",
						stored.Source, wantSource)
				}
				_, kept, err := s.configs.Get(t.Context(), plain)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case origin.code == httpjson.CodeUnsealedRevision && stored.ParentID != plain:
					t.Errorf("the replacement names parent %q, want this node's own "+
						"unsealed copy %s", stored.ParentID, plain)
				case origin.code == httpjson.CodeUnsealedFleetRevision &&
					(kept || stored.ParentID != ""):
					t.Errorf("the fleet's unsealed body was kept (%v) or named as "+
						"parent %q in this node's history", kept, stored.ParentID)
				}

				// AND IT READS: the configuration is this node's again.
				get := s.do(t, http.MethodGet, "/config", "", nil)
				if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "Acme Restored") {
					t.Errorf("GET /config after the replacement = %d: %s", get.Code, get.Body)
				}
			})
		}
	}
}

// ONLY A WHOLE DOCUMENT REPLACES ONE: every other write is built from the
// active document, so over one nothing can read it is refused as every reader
// is, under the same code, and the hint names what does work.
//
// A patch merges onto the document, an entity write splices into it, the
// charter's write edits it, a reload re-publishes it and a revert is judged on
// what it changes from it, so none of them can be built on a revision read by
// nothing.
//
// Mutation: let every write through the unsealed arm and these are no longer
// the 409 naming the remedy; answer the old hint, which named PUT /config but
// neither an admin nor a whole document, and the hint checks go red.
func TestOnlyAWholeDocumentReplacesAnUnsealedRevision(t *testing.T) {
	t.Parallel()
	for _, origin := range unsealedOrigins {
		t.Run(origin.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			earlier := s.seed(t, heldDoc)
			plain := origin.seed(t, s, heldDoc)
			for _, req := range []struct {
				method, path, body string
				headers            map[string]string
			}{
				{http.MethodPatch, "/config", `{"mission":"x"}`, summaryHeader},
				{http.MethodPut, "/config/roles/ceo",
					`{"name":"CEO","handle":"ceo","llm":"zulu","goal":"ship"}`, summaryHeader},
				{http.MethodPut, "/config/charter",
					`{"mission":"x","vision":"","policies":[]}`,
					map[string]string{"If-Match": "*"}},
				{http.MethodPost, "/config/reload", "", nil},
				{http.MethodPost, "/config/revisions/" + earlier + "/revert", "", nil},
			} {
				res := s.do(t, req.method, req.path, req.body, req.headers)
				body := refusedUnreadable(t, res, origin.code, plain, "PUT /config")
				hint, _ := body["hint"].(string)
				for _, says := range []string{"an admin", "whole document",
					"crewlet config import"} {
					if !strings.Contains(hint, says) {
						t.Errorf("%s %s: the hint does not say %q: %q", req.method,
							req.path, says, hint)
					}
				}
				if s.pointer(t) != plain {
					t.Fatalf("%s %s moved the pointer to %s", req.method, req.path,
						s.pointer(t))
				}
			}
		})
	}
}

// A REVISION SEALED UNDER A KEY THIS KEYRING LACKS IS NOT REPLACED, by a whole
// document or anything else: its remedy is putting the key back, which
// recovers the deployment's document whole, and a replacement would discard
// every credential and field in it to a write that could not read them.
//
// Mutation: let every unreadable revision through the unsealed arm and both
// land.
func TestARevisionSealedUnderAMissingKeyIsNotReplaced(t *testing.T) {
	t.Parallel()
	t.Run("this node's own copy", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t)
		sealed := s.seedSealedUnder(t, companyDoc, testCipher(t))
		for _, query := range []string{"", "?dry_run=true", "?import=true"} {
			res := s.do(t, http.MethodPut, "/config"+query, restoredDoc, summaryHeader)
			refusedUnreadable(t, res, httpjson.CodeUnreadableRevision, sealed, "secrets.keys")
		}
		if s.pointer(t) != sealed {
			t.Errorf("the pointer moved to %s", s.pointer(t))
		}
	})
	t.Run("the fleet's pointer", func(t *testing.T) {
		t.Parallel()
		author := newSurface(t)
		sealed := author.seed(t, companyDoc)
		stranger := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = author.plane })
		res := stranger.do(t, http.MethodPut, "/config", restoredDoc, summaryHeader)
		refusedUnreadable(t, res, httpjson.CodeUnreadableFleetRevision, sealed, "secrets.keys")
		if stranger.pointer(t) != sealed {
			t.Errorf("the pointer moved to %s", stranger.pointer(t))
		}
	})
}

// A REVISION SEALED IN A FORM THIS BUILD DOES NOT READ IS NOT UNSEALED, so no
// write replaces it: not a whole document, not an import, and a dry run says
// so too.
//
// A newer build's seal is wrapped as this build's is, and the reader took
// every seal it could not parse for no seal at all: GET /config called it a
// body stored without a seal, and PUT /config, which replaces an unsealed
// revision reading nothing from it, replaced the newer build's document with
// whatever the admin typed, losing every field and credential they did not.
// It is somebody's sealed document, refused under its own code with the build
// that sealed it as the remedy, from this node's own copy (a node taken back
// from a newer build) and from the fleet's pointer (a newer peer) alike.
//
// Mutation: answer a seal secrets cannot parse as unsealed, and the
// replacement lands; give it the unsealed code or hint, and the refusal sends
// the reader to a PUT.
func TestARevisionSealedInAFormThisBuildDoesNotReadIsNotReplaced(t *testing.T) {
	t.Parallel()
	newer := []byte(`{"__encrypted__":"enc:v2:k2:QUJDREVGR0hJSktMTU5PUA=="}`)
	for _, origin := range []struct {
		name, remedy string
		seed         func(t *testing.T, s *surface) string
	}{
		{"this node's own copy", "run that build on this node again",
			func(t *testing.T, s *surface) string { return s.seedPayload(t, newer) }},
		{"the fleet's pointer", "upgrade this node",
			func(t *testing.T, s *surface) string {
				return s.onThePointer(t, "newer-seal-on-the-pointer", newer)
			}},
	} {
		t.Run(origin.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			sealed := origin.seed(t, s)
			before, err := s.configs.List(t.Context(), 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, req := range []struct{ method, path string }{
				{http.MethodGet, "/config"},
				{http.MethodPut, "/config"},
				{http.MethodPut, "/config?import=true"},
				{http.MethodPut, "/config?dry_run=true"},
			} {
				res := s.do(t, req.method, req.path, restoredDoc, summaryHeader)
				body := refusedUnreadable(t, res, httpjson.CodeUnknownSealRevision,
					sealed, origin.remedy)
				said := str(body["detail"]) + " " + str(body["hint"])
				for _, never := range []string{"without a seal", "PUT /config",
					"whole document"} {
					if strings.Contains(said, never) {
						t.Errorf("%s %s says %q, which sends the reader to replace a "+
							"document somebody sealed: %s", req.method, req.path, never, said)
					}
				}
			}
			after, err := s.configs.List(t.Context(), 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			if s.pointer(t) != sealed || len(after) != len(before) {
				t.Errorf("a refused write moved the pointer to %s or stored %d "+
					"revisions (from %d)", s.pointer(t), len(after), len(before))
			}
		})
	}
}

// A MASKED CREDENTIAL HAS NOTHING TO BE RESTORED FROM, so it is refused naming
// the field and saying to write the value itself.
//
// A mask is restored from the revision a write replaces, and an unsealed one
// is read by nothing. Left to the validator, the marker was refused in words
// about a member that is new or renamed, sending whoever sent it looking for a
// rename they never made; what they need is the value.
//
// AND EVERY OTHER FAULT IS REPORTED BESIDE IT, as over a revision that opens:
// the mask refusal used to answer alone, so a document with a masked key and a
// blank name was told only about the key, under a hint saying the whole
// document is validated.
//
// Mutation: drop the check and the validator's words come back; answer the
// mask refusal before the rules, and the blank name goes unreported.
func TestAMaskOverAnUnsealedRevisionIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	masked := strings.Replace(restoredDoc, "token: sk-literal",
		`token: "`+config.Redacted+`"`, 1)
	if masked == restoredDoc {
		t.Fatal("the fixture carries no literal key to mask")
	}
	masked = strings.Replace(masked, "name: Acme Restored", `name: ""`, 1)
	if strings.Contains(masked, "Acme Restored") {
		t.Fatal("the fixture carries no name to blank")
	}
	for _, origin := range unsealedOrigins {
		t.Run(origin.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			plain := origin.seed(t, s, companyDoc)
			res := s.do(t, http.MethodPut, "/config", masked, summaryHeader)
			body := decode(t, res)
			if res.Code != http.StatusBadRequest ||
				body["error"] != string(httpjson.CodeValidationError) {
				t.Fatalf("a masked credential over %s = %d %v, want 400 "+
					"validation_error", origin.name, res.Code, body)
			}
			problems, _ := body["problems"].([]any)
			byPath := map[string]string{}
			for _, p := range problems {
				problem, _ := p.(map[string]any)
				byPath[str(problem["path"])] = str(problem["message"])
			}
			if len(problems) != 2 || len(byPath) != 2 {
				t.Fatalf("problems = %v, want the masked key and the blank name, "+
					"each once", problems)
			}
			message := byPath["providers.llm.zulu.auth.token"]
			if !strings.Contains(message, "stored without a seal") ||
				!strings.Contains(message, "Write the real value") ||
				strings.Contains(message, "renamed") {
				t.Errorf("the masked key is %q, want it told to write the value, "+
					"since nothing is read from the revision", message)
			}
			if !strings.Contains(byPath["name"], "missing") {
				t.Errorf("the blank name is %q, want it reported beside the mask",
					byPath["name"])
			}
			if s.pointer(t) != plain {
				t.Errorf("a refused write moved the pointer to %s", s.pointer(t))
			}
		})
	}
}

// EVERY HELD SEAT IS KEPT, though nothing says which seats the replaced
// revision had.
//
// A write is refused for taking a human seat away while somebody holds it,
// and what it takes away is read off the revision it replaces. One read by
// nothing answered "nothing leaves", so a replacement could drop every seat a
// colleague, a service account or an open invitation held. Instead it asks the
// directory about every seat its own document does not keep as a human seat,
// and refuses any held one, a seat no document of this company names any more
// included. The CONTROL keeps every held seat and lands, having asked about
// exactly the seats it does not keep; and a directory it cannot read refuses it
// naming no seats, since it can list none.
//
// Mutation: judge against the missing prior and every arm but the control
// lands; ask about the seats the document keeps and the control is refused.
func TestReplacingAnUnsealedRevisionKeepsEveryHeldSeat(t *testing.T) {
	t.Parallel()
	inviteSam := configapi.SeatHolder{Invitation: "018f3a9c-4d2e-7000-8000-00000000a5a1",
		ExpiresAt: pinned.Add(6 * 24 * time.Hour)}
	held := func() map[string]configapi.SeatHolder {
		return map[string]configapi.SeatHolder{
			"jane": {Person: "p-jane", Kind: iam.KindPerson, Login: "jane.doe",
				Stage: iam.StageActive},
			"sam": inviteSam,
			"ana": {Person: "p-ana", Kind: iam.KindPerson, Login: "ana.ruiz",
				Stage: iam.StageSuspended},
		}
	}
	for _, origin := range unsealedOrigins {
		t.Run(origin.name, func(t *testing.T) {
			t.Parallel()
			for _, c := range []struct {
				name, doc, seat, holder string
				extra                   map[string]configapi.SeatHolder
			}{
				{"a held person's seat dropped", withoutSeat(t, "Jane Doe"), "jane",
					"jane.doe", nil},
				{"an invited seat made an agent's", strings.Replace(heldDoc,
					"    handle: sam\n    kind: human", "    handle: sam\n    llm: zulu", 1),
					"sam", "", nil},
				{"a seat no document names any more", heldDoc, "ghost", "ci:release",
					map[string]configapi.SeatHolder{"ghost": {Person: "p-ci",
						Kind: iam.KindMachine, Login: "ci:release", Stage: iam.StageActive}}},
			} {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					s := newSurface(t)
					s.held = held()
					for seat, h := range c.extra {
						s.held[seat] = h
					}
					plain := origin.seed(t, s, heldDoc)
					res := s.do(t, http.MethodPut, "/config", c.doc, summaryHeader)
					body := decode(t, res)
					entries, _ := body["held"].(map[string]any)
					entry, _ := entries[c.seat].(map[string]any)
					if res.Code != http.StatusConflict ||
						body["error"] != string(httpjson.CodeSeatHeld) || entry == nil {
						t.Fatalf("= %d %v, want 409 seat_held naming %s", res.Code, body, c.seat)
					}
					if c.holder != "" && entry["login"] != c.holder {
						t.Errorf("%s is held by %v, want %s", c.seat, entry, c.holder)
					}
					if c.holder == "" && entry["invitation"] != inviteSam.Invitation {
						t.Errorf("%s is held by %v, want the invitation", c.seat, entry)
					}
					if len(entries) != 1 {
						t.Errorf("the refusal names %v, want %s alone", entries, c.seat)
					}
					// WORDED FOR A REPLACEMENT: it cannot say the seat was in
					// the revision it replaces, and keeping the seat is the
					// remedy that needs no company running here.
					detail, hint := str(body["detail"]), str(body["hint"])
					if !strings.Contains(detail, "doesn't keep these human seats") ||
						strings.Contains(detail, "removes") ||
						!strings.HasPrefix(hint, "Keep each seat in the document as a "+
							"human seat") {
						t.Errorf("the refusal says %q, hint %q, want the seats the "+
							"document doesn't keep and keeping them offered first",
							detail, hint)
					}
					if s.pointer(t) != plain {
						t.Errorf("a refused write moved the pointer to %s", s.pointer(t))
					}
				})
			}

			t.Run("the control: every held seat kept", func(t *testing.T) {
				t.Parallel()
				s := newSurface(t)
				s.held = held()
				origin.seed(t, s, heldDoc)
				res := s.do(t, http.MethodPut, "/config", heldDoc, summaryHeader)
				if res.Code != http.StatusCreated {
					t.Fatalf("a document keeping every held seat = %d: %s", res.Code, res.Body)
				}
				seats := append(slices.Clone(heldHandles), "ghost")
				if len(s.asked) != 1 || !slices.Equal(s.admitted(0, seats...),
					[]string{"ceo", "cto", "ghost"}) {
					t.Errorf("the directory was asked %d times, the first about %v of %v, "+
						"want once about every seat the document does not keep as a "+
						"human seat", len(s.asked), s.admitted(0, seats...), seats)
				}
			})

			t.Run("an unreadable directory refuses it naming no seats", func(t *testing.T) {
				t.Parallel()
				s := newSurface(t)
				s.directoryDown = errDirectoryDown
				plain := origin.seed(t, s, heldDoc)
				res := s.do(t, http.MethodPut, "/config", heldDoc, summaryHeader)
				body := decode(t, res)
				detail, _ := body["detail"].(string)
				if res.Code != http.StatusServiceUnavailable ||
					body["error"] != string(httpjson.CodeSeatHoldersUnavailable) {
					t.Fatalf("= %d %v, want 503 seat_holders_unavailable", res.Code, body)
				}
				if _, named := body["seats"]; named ||
					!strings.Contains(detail, "replaces a revision this node can't read") ||
					!strings.Contains(detail, "nothing was written") {
					t.Errorf("the refusal is %v, want no seats and a detail saying "+
						"why every held seat is checked and that nothing was written", body)
				}
				if s.pointer(t) != plain {
					t.Errorf("a refused write moved the pointer to %s", s.pointer(t))
				}
			})
		})
	}
}

// ONLY AN ADMIN REPLACES ONE. A lead's write is decided on what it changes,
// and against a revision read by nothing it changes everything: every setting
// and every seat and unit it names, so it reaches outside what they lead and
// is refused naming admin, even where the same document over a revision that
// opens changes their own team alone (see
// TestALeadsWholeDocumentWritesAreJudgedTheSameWay). The CONTROL is that
// document from an admin.
//
// Mutation: admit a lead's write over an unsealed revision as though it
// replaced the document it read, and the lead's write lands.
func TestOnlyAnAdminReplacesAnUnsealedRevision(t *testing.T) {
	t.Parallel()
	inside := strings.Replace(leadDoc, "build tools", "build better tools", 1)
	for _, origin := range unsealedOrigins {
		t.Run(origin.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			plain := origin.seed(t, s, leadDoc)
			res := doAs(t, s, platformLead(), http.MethodPut, "/config",
				string(companyJSON(t, inside)), map[string]string{"X-Summary": "tools"})
			parts := refusedParts(t, res)
			if !slices.ContainsFunc(parts, func(p string) bool {
				return strings.HasPrefix(p, "setting/name/")
			}) {
				t.Errorf("refused %v, want the company's settings among them", parts)
			}
			if s.pointer(t) != plain {
				t.Fatalf("a lead's refused write moved the pointer to %s", s.pointer(t))
			}
			if res := s.do(t, http.MethodPut, "/config", inside, summaryHeader); res.Code !=
				http.StatusCreated {
				t.Errorf("the same document from an admin = %d: %s", res.Code, res.Body)
			}
		})
	}
}

// ONLY A SEAT THE WRITE ASKED ABOUT IS REFUSED, whatever the directory
// answers: a seat the document keeps is no seat it takes from anybody.
//
// [configapi.Holders] answers about the seats its predicate admits, and the
// directory's own reader narrows its claims to them; a seam that answered
// more would refuse a write for a seat it keeps. Asked here of a directory
// that answers EVERY binding, over a revision that opens (one seat removed,
// the others kept) and over an unsealed one (every held seat kept).
//
// Mutation: refuse every seat the directory answered, and the removal names
// the kept seats too and the replacement is refused.
func TestOnlyTheSeatsAWriteAskedAboutAreRefused(t *testing.T) {
	t.Parallel()
	everyBinding := func(o *configapi.Options) {
		o.Holders = func(context.Context, func(string) bool, time.Time) (
			map[string]configapi.SeatHolder, error) {

			return map[string]configapi.SeatHolder{
				"jane": {Person: "p-jane", Kind: iam.KindPerson, Login: "jane.doe",
					Stage: iam.StageActive},
				"sam": {Person: "p-sam", Kind: iam.KindPerson, Login: "sam.lee",
					Stage: iam.StageActive},
				"ana": {Person: "p-ana", Kind: iam.KindPerson, Login: "ana.ruiz",
					Stage: iam.StageActive},
			}, nil
		}
	}
	t.Run("a removal over a revision that opens", func(t *testing.T) {
		t.Parallel()
		s := newSurfaceWith(t, everyBinding)
		before := s.seed(t, heldDoc)
		res := s.do(t, http.MethodPut, "/config", withoutSeat(t, "Jane Doe"), summaryHeader)
		heldEntry(t, s, before, res.Code, res.Body.String(), "jane")
		if entries, _ := decode(t, res)["held"].(map[string]any); len(entries) != 1 {
			t.Errorf("the refusal names %v, want jane alone: sam and ana are kept",
				entries)
		}
	})
	for _, origin := range unsealedOrigins {
		t.Run("every held seat kept over "+origin.name, func(t *testing.T) {
			t.Parallel()
			s := newSurfaceWith(t, everyBinding)
			origin.seed(t, s, heldDoc)
			if res := s.do(t, http.MethodPut, "/config", heldDoc, summaryHeader); res.Code !=
				http.StatusCreated {
				t.Errorf("a document keeping every held seat = %d: %s", res.Code, res.Body)
			}
		})
	}
}

// errDirectoryDown is a directory this node cannot read.
var errDirectoryDown = errors.New("the identity estate is not open")

// sep is what joins another query parameter onto query.
func sep(query string) string {
	if query == "" {
		return "?"
	}
	return "&"
}
