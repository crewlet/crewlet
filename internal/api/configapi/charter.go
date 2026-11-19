package configapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/words"
)

// THE CHARTER: the company's mission, vision and standing policies, one
// resource (`GET` and `PUT /config/charter`) that Company › Principles edits.
// The write is a merge patch of the three fields through [Service.prepare] and
// [Service.commit], the one pipeline every write takes.

// # Why a route of its own rather than PATCH /config
//
//   - A PRECONDITION ON THE CHARTER, not the document: PATCH /config's
//     If-Match is the whole revision, so a draft would be refused because
//     somebody connected an integration meanwhile. The entity-tag digests the
//     charter's own content ([charterTag]); a revision that moved for another
//     reason is merged onto ([charterAttempts]).
//   - A CONFLICT THAT CARRIES THE CHARTER NOW, shown beside the refused draft,
//     not a revision id sending the editor to the whole company document.
//   - A SUMMARY OF ITS OWN, derived from what changed ([charterSummary]).

// # Admin's, on verbs of its own
//
// Reads are `config.principles.read` and writes `config.principles.write`,
// decided as `config.read` and `config.write` are: admin's, the write asking a
// recent proof. The charter is not a lead's: it is the company's root, nobody's
// subtree (admission.go).
//
// THE VERBS ARE THEIR OWN so the authority table can say what it says of no
// other part of the document: the Dashboard Assistant reads it as the person
// and writes it only as the one request its owner approved from a rendered
// before-and-after, at the same step-up window (ADR-0052, ADR-0051). This route
// is that write path for both (a PUT with the If-Match its preview read).

// charterAttempts is how many times a write is tried when the company document
// moves under it for a reason that is not the charter.
//
// THE PRECONDITION IS THE CHARTER'S, so a revision activated between this
// write's read and its compare-and-set by another writer (a reconcile pass, an
// added seat) is no conflict the person can resolve: their draft still applies.
// Each attempt re-reads the active revision and judges the precondition again,
// so a charter that DID move is refused naming it. Three, because each lost
// attempt is a whole write landing within one save, and a fourth in a row
// means a writer that will not let up, for the person to hear about (409
// revision_advanced) rather than a loop to keep losing to.
const charterAttempts = 3

// charterTag is the strong entity-tag of a charter: a digest of what it says.
//
// THE CONTENT, NOT THE REVISION, because the precondition a person editing the
// charter holds is "the charter is still what I started from". Strong, because
// it names exactly the bytes [Service.getCharter] answers, which carry nothing
// but the charter.
func charterTag(ch config.Charter) string {
	encoded, err := json.Marshal(ch)
	if err != nil {
		// A struct of strings always encodes; a failure is a broken build.
		panic(fmt.Sprintf("configapi: encode a charter: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return `"charter-` + hex.EncodeToString(sum[:16]) + `"`
}

// charterBody is a charter as a write sends it: every field required, so a
// client that forgot one is told so rather than having it cleared.
type charterBody struct {
	Mission  *string   `json:"mission"`
	Vision   *string   `json:"vision"`
	Policies *[]string `json:"policies"`
}

// errCharterField is a charter write missing one of its three fields.
var errCharterField = errors.New("a charter names all three of mission, vision and " +
	`policies; send "" or [] for one that says nothing`)

// readCharter decodes a charter write strictly: one JSON object, the three
// fields and no others.
func readCharter(body []byte) (config.Charter, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var sent charterBody
	if err := decoder.Decode(&sent); err != nil {
		return config.Charter{}, errors.New(plainjson.Unreadable("the body", err))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return config.Charter{}, errors.New(plainjson.Unreadable("the body", plainjson.ErrTrailing))
	}
	if sent.Mission == nil || sent.Vision == nil || sent.Policies == nil {
		return config.Charter{}, errCharterField
	}
	return config.Charter{
		Mission: *sent.Mission, Vision: *sent.Vision, Policies: *sent.Policies,
	}.Trimmed(), nil
}

// charterPatch is the merge patch that turns any document's charter into ch.
//
// An empty field is NULL rather than an empty value, so it is removed from the
// document: the company document leaves out a mission nobody wrote, and a patch
// writing `""` would make the stored document say something no reader of it
// sees.
func charterPatch(ch config.Charter) ([]byte, error) {
	patch := map[string]any{"mission": nil, "vision": nil, "policies": nil}
	if ch.Mission != "" {
		patch["mission"] = ch.Mission
	}
	if ch.Vision != "" {
		patch["vision"] = ch.Vision
	}
	if len(ch.Policies) > 0 {
		patch["policies"] = ch.Policies
	}
	return json.Marshal(patch)
}

// charterSummary is the revision summary a charter write records when its
// caller names none: which of the three it changed.
func charterSummary(was, is config.Charter) string {
	var parts []words.Said
	if was.Mission != is.Mission {
		parts = append(parts, "mission")
	}
	if was.Vision != is.Vision {
		parts = append(parts, "vision")
	}
	if !slices.Equal(was.Policies, is.Policies) {
		parts = append(parts, "policies")
	}
	if len(parts) == 0 {
		return "left the company's principles as they were"
	}
	return "edited the company's " + words.And(parts)
}

// getCharter serves GET /config/charter: the active document's charter, its
// tag in ETag.
func (s *Service) getCharter(w http.ResponseWriter, r *http.Request) {
	company, _, err := s.documentOf(r.Context())
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		refuseNoCompany(w)
		return
	case err != nil:
		if !RefuseUnreadable(w, err) {
			s.fail(w, r, "read the active revision", err)
		}
		return
	}
	charter := company.Charter()
	w.Header().Set("ETag", charterTag(charter))
	writeJSON(w, http.StatusOK, charter)
}

// putCharter serves PUT /config/charter: the charter replaced, as a new
// revision of the company document.
//
// With `dry_run=true` it is the same request, judged the same way, that stores
// and activates nothing.
func (s *Service) putCharter(w http.ResponseWriter, r *http.Request) {
	if s.refusedManaged(w, r) {
		return
	}
	dryRun, ok := dryRunOf(w, r)
	if !ok {
		return
	}
	// REQUIRED, unlike on the whole document: a script that owns the whole
	// document has no race to lose, and an editor of the charter always has
	// one, since it is the field every administrator can edit from a card.
	// `*` stays the honest spelling of "whatever it says now".
	expected := strings.TrimSpace(r.Header.Get("If-Match"))
	if expected == "" {
		httpjson.FailWith(w, http.StatusPreconditionRequired, httpjson.CodeBadIfMatch,
			map[string]string{
				"detail": "PUT /config/charter needs If-Match: the ETag of the " +
					"charter you edited, from GET /config/charter",
			})
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		refuseBody(w, err)
		return
	}
	next, err := readCharter(body)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return
	}
	principal, ok := principalOf(w, r)
	if !ok {
		return
	}
	by := attributionOf(r)
	summary := strings.TrimSpace(r.Header.Get("X-Summary"))

	var raced error
	for range charterAttempts {
		done, err := s.writeCharter(w, r, charterWrite{
			expected: expected, next: next, dryRun: dryRun, summary: summary,
			by: by, principal: principal,
		})
		var lost *RacedError
		if errors.As(err, &lost) {
			raced = err
			continue
		}
		if err != nil {
			s.refuseApply(w, r, err)
			return
		}
		if done {
			return
		}
	}
	s.refuseApply(w, r, raced)
}

// charterWrite is one attempt of a charter write.
type charterWrite struct {
	expected  string
	next      config.Charter
	dryRun    bool
	summary   string
	by        iam.Actor
	principal *iam.Principal
}

// writeCharter makes one attempt: it judges the precondition against the
// charter active now, and writes. done reports that the request has been
// answered; a [*RacedError] is a revision that moved for another reason, which
// the caller tries again; any other error is the caller's to answer.
func (s *Service) writeCharter(w http.ResponseWriter, r *http.Request, c charterWrite) (done bool, err error) {
	ctx := r.Context()
	cur, found, err := s.current(ctx)
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrNoActiveRevision
	}
	active := cur.revision
	prior, err := s.open(cur)
	if err != nil {
		if RefuseUnreadable(w, err) {
			return true, nil
		}
		return false, fmt.Errorf("configapi: open the active revision: %w", err)
	}
	current := prior.Charter()
	tag := charterTag(current)
	if current.Equal(c.next) {
		// NOTHING TO WRITE, and nothing is: a revision that changes nothing
		// still re-activates on every node and rebuilds every seat.
		//
		// ASKED BEFORE THE PRECONDITION, which is what makes a write safe to
		// send again: a save whose answer was lost is retried with the tag it
		// was built on, which no longer matches the charter it wrote, and the
		// charter already saying exactly what the write asks for is the write
		// having happened. Nothing is overwritten, so there is nothing for
		// the precondition to protect.
		w.Header().Set("ETag", tag)
		answer := map[string]any{"charter": current, "changed": false}
		if c.dryRun {
			answer["valid"] = true
		} else {
			answer["revision_id"] = active.ID
		}
		writeJSON(w, http.StatusOK, answer)
		return true, nil
	}
	if !matchesTag(c.expected, tag) {
		// 412, the answer RFC 9110 gives a failed If-Match, with the charter
		// as it stands: the editor shows the person their draft beside what
		// replaced it, and resends with this tag to keep theirs.
		w.Header().Set("ETag", tag)
		httpjson.FailWithFields(w, http.StatusPreconditionFailed, httpjson.CodeVersionMoved,
			httpjson.Detail{
				"current": current, "etag": tag,
				"hint": "the principles changed after you started editing; " +
					"compare, then send your draft again with this etag to keep it",
			})
		return true, nil
	}
	patch, err := charterPatch(c.next)
	if err != nil {
		return false, fmt.Errorf("configapi: encode the charter: %w", err)
	}
	summary := c.summary
	if summary == "" {
		summary = charterSummary(current, c.next)
	}
	d := patchDraft(ApplyRequest{Patch: patch, Summary: summary, By: c.by, Expect: active.ID}, nil)
	d.principal, d.charter = c.principal, true
	prepared, err := s.prepare(ctx, d)
	if err != nil {
		return false, err
	}
	if c.dryRun {
		if err = s.checks(prepared, summary); err != nil {
			return false, err
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"valid": true, "charter": c.next, "changed": true,
		})
		return true, nil
	}
	applied, err := s.commit(ctx, prepared, summary)
	if err != nil {
		return false, err
	}
	w.Header().Set("ETag", charterTag(c.next))
	writeJSON(w, http.StatusOK, map[string]any{
		"charter": c.next, "changed": true,
		"revision_id": applied.RevisionID, "epoch": applied.Epoch,
	})
	return true, nil
}
