package seataccess

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/vendortext"
	"github.com/crewlet/crewlet/internal/words"
)

// IdentityGuide is the sheet that sets up a seat's own identity at a vendor,
// in the vendor's words: the same three steps for every vendor (at the
// vendor, back here, the check), so every integration's box draws one sheet.
type IdentityGuide struct {
	// Vendor is the vendor as its own name ("Apple", "Meta", "Google Play"),
	// and Noun what the seat's identity is there ("key", "system user").
	Vendor string `json:"vendor"`
	Noun   string `json:"noun"`

	// Records is what the vendor then records under the seat ("its
	// submissions"), and Why the one line on what the identity changes.
	Records string `json:"records"`
	Why     string `json:"why"`

	// Company is the company's identity there ("the company's team keys"),
	// empty where the vendor has none to share.
	Company string `json:"company,omitempty"`

	// Name is the name to give the identity ([seatname.For]), NameRule the
	// vendor's rule under it as the sheet says it, and NameWhat what is
	// named.
	Name     string `json:"name"`
	NameRule string `json:"name_rule"`
	NameWhat string `json:"name_what"`

	// Role is the vendor role the seat's level needs, and Never the role
	// Crewlet refuses for a seat ("Admin").
	Role  string `json:"role"`
	Never string `json:"never,omitempty"`

	// Steps are the person's steps at the vendor, with its exact labels
	// marked and each page linked on the first step that names it.
	Steps []GuideStep `json:"steps"`

	// Entries are the seat's scope entries to add the identity to, each with
	// the vendor's page that adds it there and, once checked, where it stands;
	// nil where the vendor has no entries.
	Entries []IdentityEntry `json:"entries,omitempty"`

	// Wider is adding the identity once to what holds the entries (an
	// Analytics account) instead; nil where the vendor has no such place.
	Wider *GuideWider `json:"wider,omitempty"`

	// ReadyAt is when the vendor can find the identity, while it is
	// [IdentityPreparing]; zero once it can.
	ReadyAt time.Time `json:"ready_at,omitzero"`

	// Link is the vendor's page as a button under the steps, only where no
	// step names it; nil where one does.
	Link *GuideLink `json:"link,omitempty"`

	// Note is one sentence on what the vendor allows that the person should
	// know (who may make it, what it reaches).
	Note vendortext.Text `json:"note,omitempty"`

	// Back is what the person brings back.
	Back GuideBack `json:"back"`

	// Make is where Crewlet can make the identity itself; nil where it cannot.
	Make *GuideMake `json:"make,omitempty"`

	// Checks are the reads the check makes, in order.
	Checks []GuideCheck `json:"checks"`
}

// GuideWider is the alternative to adding an identity entry by entry: once
// on what holds them, which reaches every current and future entry there.
type GuideWider struct {
	Label string          `json:"label"`
	Does  vendortext.Text `json:"does"`
	// Links are the vendor's page for each holder of the seat's entries.
	Links []GuideLink `json:"links,omitempty"`
}

// GuideStep is one numbered step at the vendor; Copy the value it asks for,
// ready to copy.
type GuideStep struct {
	Text vendortext.Text `json:"text"`
	Copy *GuideCopy      `json:"copy,omitempty"`
}

// GuideCopy is a value to copy into the vendor's form, with its rule.
type GuideCopy struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Rule  string `json:"rule,omitempty"`
}

// GuideLink is a vendor page.
type GuideLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// GuideBack is what the person brings back: a file, a token, or nothing.
type GuideBack struct {
	Kind GuideBackKind `json:"kind"`

	// Accept is the file's extension (`.p8`), Hint what it is called, and
	// Label the field a file's name fills (`Key ID`), for a file.
	Accept string          `json:"accept,omitempty"`
	Hint   vendortext.Text `json:"hint,omitempty"`
	Label  string          `json:"label,omitempty"`

	// Text is the sentence where nothing comes back.
	Text vendortext.Text `json:"text,omitempty"`

	// Detail is one optional value asked for beside a file, which the
	// person may leave empty; nil where the lane asks for none.
	Detail *GuideDetail `json:"detail,omitempty"`
}

// GuideDetail is the optional value a sheet asks for beside a file: its label
// and one line saying when it is needed.
type GuideDetail struct {
	Label string `json:"label"`
	Hint  string `json:"hint"`
}

// GuideBackKind is what a person brings back from the vendor.
type GuideBackKind string

// What a person brings back: the vendor's file, a token they paste, or
// nothing, where Crewlet reads the identity as it is.
const (
	BackFile    GuideBackKind = "file"
	BackToken   GuideBackKind = "token"
	BackNothing GuideBackKind = "nothing"
)

// GuideMake is Crewlet making the identity: what it does, or why it cannot now.
type GuideMake struct {
	Label       string          `json:"label"`
	Does        vendortext.Text `json:"does"`
	Unavailable string          `json:"unavailable,omitempty"`
	// Reads are what making it does before the check's reads, as its check lists them.
	Reads []GuideCheck `json:"reads,omitempty"`
}

// GuideCheck is one read the check makes.
type GuideCheck struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// IdentityHolds is whose identity signs a seat's calls, as the box says it.
type IdentityHolds string

// Whose identity signs: the seat's own, the company's, or none.
const (
	HoldsOwn     IdentityHolds = "own"
	HoldsCompany IdentityHolds = "company"
	// HoldsNone is a seat whose calls are refused: its own identity no
	// longer works and the company's is never put in its place.
	HoldsNone IdentityHolds = "none"
)

// IdentityAnswer is a seat's identity at a lane on the access answer: what the
// box draws and Ask Crewlet reads, worded here once.
type IdentityAnswer struct {
	// Offered is whether the vendor offers the seat an identity of its own.
	// Where it does not, the company's is the lane's normal state and there
	// is nothing to set up.
	Offered bool `json:"offered"`

	// Mode is the document's setting, Holds who signs now.
	Mode  IdentityMode  `json:"mode"`
	Holds IdentityHolds `json:"holds"`

	// Status is the chip, StatusLabel its word.
	Status      IdentityStatus `json:"status"`
	StatusLabel string         `json:"status_label"`

	// Name and ID are the kept identity as the vendor names it; Role and
	// Team what the last check read back; Needs the role the level needs.
	Name  string `json:"name,omitempty"`
	ID    string `json:"id,omitempty"`
	Role  string `json:"role,omitempty"`
	Needs string `json:"needs,omitempty"`
	Team  string `json:"team,omitempty"`

	// CheckedAt is when the identity was last checked.
	CheckedAt time.Time `json:"checked_at,omitzero"`

	// Said is the box's one line: who the vendor sees, and what to do.
	Said string `json:"said"`

	// Problem is what needs a person, a title and a sentence; nil where
	// nothing does.
	Problem *IdentityProblem `json:"problem,omitempty"`

	// Blocked is why the vendor cannot give the seat its own now.
	Blocked string `json:"blocked,omitempty"`

	// SetUp, Replace and UseCompany are the actions the box offers.
	SetUp      bool `json:"set_up"`
	Replace    bool `json:"replace"`
	UseCompany bool `json:"use_company"`

	// Retiring are the identities no longer used, to revoke at the vendor.
	Retiring []RetiringIdentity `json:"retiring,omitempty"`

	// Guide is the sheet, nil where nothing is offered.
	Guide *IdentityGuide `json:"guide,omitempty"`
}

// IdentityProblem is what needs a person in an agent's box.
type IdentityProblem struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// IdentityOf is the seat's identity at lane for the access answer, or nil
// where the lane says nothing of one. On a lane where each seat holds an
// account of its own (an Expo robot), the identity is that account, kept once
// its credential is sealed ([AccountIdentity]); have is what the vendor showed.
func IdentityOf(v Vendor, c *config.Company, seat SeatRef, lane Lane, want Desired, have Observed,
	rec *IdentityRecord, now time.Time) *IdentityAnswer {

	spec, ok := v.Catalog().Lane(lane)
	if !ok || !spec.Identity.Declared() {
		return nil
	}
	if !spec.EngineHeld {
		rec = AccountIdentity(have)
	}
	is := spec.Identity
	out := &IdentityAnswer{Offered: is.Offered(), Mode: is.ModeOf(want)}
	var g IdentityGuide
	if guider, ok := v.(IdentityGuider); ok && is.Offered() && c != nil {
		g = guider.IdentityGuide(c, seat, lane, want)
		if is.Standing {
			standingGuide(&g, rec)
		}
		out.Guide = &g
	}
	out.Needs = g.Role
	agent := seat.Label()
	vendor := firstNonEmpty(g.Vendor, KindName(lane.Kind()))
	noun := firstNonEmpty(g.Noun, "identity")
	company := firstNonEmpty(g.Company, "the company's identity")
	switch {
	case !is.Offered():
		out.Holds, out.Status = HoldsCompany, IdentityCompanys
		out.Said = vendor + " sees " + company + " for every agent."
	case out.Mode == IdentityCompany:
		out.Holds, out.Status = HoldsCompany, IdentityCompanys
		out.SetUp = true
		if rec != nil && len(rec.Retiring) > 0 {
			out.Retiring = rec.Retiring
			out.Problem = retiringProblem(rec.Retiring, vendor, noun)
		}
		out.Said = "Needs you: set up " + agent + "'s own " + noun + " so " + vendor + " records " +
			firstNonEmpty(g.Records, "its actions") + ". Until then " + vendor + " sees " + company + "."
	default:
		identityOwn(out, is, rec, agent, vendor, noun, company, now)
	}
	if out.Blocked != "" {
		out.SetUp = false
	}
	out.StatusLabel = out.Status.Label()
	return out
}

// identityOwn words a seat set to its own identity.
func identityOwn(out *IdentityAnswer, is IdentitySpec, rec *IdentityRecord, agent, vendor, noun,
	company string, now time.Time) {

	status := rec.Status()
	if last := lastCheck(rec); status == IdentityPreparing && last != nil && !last.ReadyAt.IsZero() &&
		!now.Before(last.ReadyAt) {
		// SETTLED SINCE THE CHECK: the vendor can find it now, so it is the person's to add.
		status = IdentityNone
		if out.Guide != nil {
			out.Guide.ReadyAt = time.Time{}
		}
	}
	out.Status = status
	if rec != nil {
		out.Retiring = rec.Retiring
		out.Name, out.ID = rec.Name, rec.ID
		if check := rec.Check; rec.Kept() && check != nil {
			out.Role, out.Team, out.CheckedAt = check.Role, check.Team, check.At
			out.Needs = firstNonEmpty(check.Needs, out.Needs)
		}
		if a := rec.Attempt; a != nil {
			out.Blocked = a.Blocked
			if !rec.Kept() {
				out.Role, out.Team, out.CheckedAt = a.Role, a.Team, a.At
				out.Name = firstNonEmpty(out.Name, a.Name)
			}
		}
	}
	switch {
	case status.works():
		// A STANDING IDENTITY IS THE SEAT'S FOR GOOD: nothing replaces it.
		out.Holds, out.Replace = HoldsOwn, !is.Standing
		out.Said = vendor + " sees " + agent + "'s own " + noun
		if out.Name != "" {
			out.Said += ", " + out.Name
		}
		out.Said += "."
		if status == IdentityReplacing && len(out.Retiring) > 0 {
			out.Problem = retiringProblem(out.Retiring, vendor, noun)
		}
		if check := lastCheck(rec); status == IdentityUnchecked && check != nil {
			out.Said = strings.TrimSpace(check.Said + " " + check.Remedy)
		}
	case rec.Kept():
		// THE KEPT IDENTITY STOPPED WORKING: calls stop, never the company's.
		out.Holds, out.SetUp = HoldsNone, true
		out.UseCompany = is.Company
		out.Said = "Refused: " + vendor + " no longer accepts " + agent + "'s own " + noun +
			", so its calls stop until it has a working one."
		out.Problem = problemOf(rec.Check, vendor)
	default:
		out.Holds, out.SetUp = HoldsCompany, true
		switch {
		case !is.Company && is.Credential != "":
			// SEALED APART FROM THE LANE (App Store Connect's key): held only once kept.
			out.Holds = HoldsNone
		case !is.Company:
			out.Holds = HoldsOwn
		}
		out.Said = "Needs you: set up " + agent + "'s own " + noun + " so " + vendor + " records its actions."
		if is.Company {
			out.Said += " Until then " + vendor + " sees " + company + "."
		}
		if is.Standing {
			out.Said = standingSaid(status, agent, vendor, noun)
		}
		switch {
		case status == IdentityPreparing && rec.Attempt != nil && rec.Attempt.Said != "":
			// THE VENDOR'S WAIT, NOT A PROBLEM: the check says how long.
			out.Said = "Getting ready: " + rec.Attempt.Said
		case rec != nil && rec.Attempt != nil && (status != IdentityNone || rec.Attempt.Said != ""):
			out.Problem = problemOf(rec.Attempt, vendor)
		}
		if out.Blocked != "" {
			out.Said = out.Blocked + " " + vendor + " sees " + company + "."
		}
	}
}

// retiringProblem asks a person to revoke the identities Crewlet no longer
// uses.
func retiringProblem(rs []RetiringIdentity, vendor, noun string) *IdentityProblem {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, firstNonEmpty(r.ID, r.Name))
	}
	if len(names) == 1 {
		return &IdentityProblem{Title: "Revoke the old " + noun + " at " + vendor,
			Body: names[0] + " is no longer used. Revoke it at " + vendor + "; Crewlet clears this once " +
				vendor + " refuses it."}
	}
	return &IdentityProblem{Title: "Revoke the old " + noun + "s at " + vendor,
		Body: words.And(names) + " are no longer used. Revoke them at " + vendor + "; Crewlet clears each once " +
			vendor + " refuses it."}
}

// AccountIdentity is the identity record of a seat whose identity is its own
// account at the vendor: kept while the account exists with a credential
// sealed for it, refused where the vendor shows that credential gone.
func AccountIdentity(have Observed) *IdentityRecord {
	if !have.Exists || have.Credential == nil {
		return nil
	}
	check := &IdentityCheck{Status: IdentityReady, ID: have.AccountID, Name: have.Account}
	switch {
	case have.CredentialGone:
		check.Status, check.Said = IdentityRefused, "The vendor no longer accepts its credential."
	case !have.Enabled:
		check.Status, check.Said = IdentityRefused, "Its account is disabled at the vendor."
	}
	id := firstNonEmpty(have.AccountID, have.Account, have.Credential.Variable)
	return &IdentityRecord{ID: id, Name: have.Account, Check: check, Since: have.Credential.MintedAt}
}

// standingSaid is the box's line for a standing identity that does not pass:
// it exists already, so what a person does depends on what the check found.
func standingSaid(status IdentityStatus, agent, vendor, noun string) string {
	switch status {
	case IdentityPreparing:
		return "Getting ready: Crewlet is making " + agent + "'s own " + noun + ". " + vendor +
			" can find it in about a minute, then add it there."
	case IdentityWrongRole:
		return "Needs you: " + agent + "'s own " + noun + " holds the wrong role in " + vendor +
			", which refuses the calls it needs more for."
	case IdentityRefused:
		return "Refused: Google does not let Crewlet sign in as " + agent + "'s own " + noun + ", so " +
			vendor + " refuses its calls."
	}
	return "Needs you: add " + agent + "'s own " + noun + " in " + vendor + ". Until then " + vendor +
		" refuses its calls."
}

// standingGuide is g as a standing identity's record tells it: the identity
// the last check named (a rename keeps the made one), and that check's
// reading of each entry and when a preparing one can be added.
func standingGuide(g *IdentityGuide, rec *IdentityRecord) {
	var name string
	switch {
	case rec.Kept():
		name = firstNonEmpty(rec.Name, rec.ID)
	case rec != nil && rec.Attempt != nil:
		name = firstNonEmpty(rec.Attempt.Name, rec.Attempt.ID)
	}
	if name != "" && name != g.Name {
		for i := range g.Steps {
			if c := g.Steps[i].Copy; c != nil && c.Value == g.Name {
				copied := *c
				copied.Value = name
				g.Steps[i].Copy = &copied
			}
		}
		g.Name = name
	}
	last := lastCheck(rec)
	if last == nil {
		return
	}
	g.Entries = overlaid(g.Entries, last.Entries)
	if last.Status == IdentityPreparing {
		g.ReadyAt = last.ReadyAt
	}
	if g.Wider != nil {
		g.Wider.Links = widerLinks(g.Entries)
	}
}

// lastCheck is the check the record's status reads ([IdentityRecord.Status]).
func lastCheck(rec *IdentityRecord) *IdentityCheck {
	switch {
	case rec.Kept() && rec.Check != nil:
		return rec.Check
	case rec != nil:
		return rec.Attempt
	}
	return nil
}

// overlaid is the guide's entries with what a check read of each laid over
// them, in the guide's order: an entry the check did not read keeps the
// guide's own words.
func overlaid(entries, checked []IdentityEntry) []IdentityEntry {
	read := map[string]IdentityEntry{}
	for _, e := range checked {
		read[e.ID] = e
	}
	out := make([]IdentityEntry, len(entries))
	for i, e := range entries {
		c, ok := read[e.ID]
		if !ok {
			out[i] = e
			continue
		}
		c.AddURL = firstNonEmpty(c.AddURL, e.AddURL)
		out[i] = c
	}
	return out
}

// widerLinks are the vendor's page for each holder of an entry the identity
// is not added to yet, once each, in the entries' order.
func widerLinks(entries []IdentityEntry) []GuideLink {
	var out []GuideLink
	seen := map[string]bool{}
	for _, e := range entries {
		if e.GroupURL == "" || seen[e.GroupURL] || e.State == EntryAdded || e.State == EntryUnchecked {
			continue
		}
		seen[e.GroupURL] = true
		out = append(out, GuideLink{Label: firstNonEmpty(e.Group, e.GroupURL), URL: e.GroupURL})
	}
	return out
}

// problemOf is a failed check as the box's callout.
func problemOf(check *IdentityCheck, vendor string) *IdentityProblem {
	if check == nil {
		return &IdentityProblem{Title: vendor + " refused it", Body: "Set up the identity again."}
	}
	body := check.Said
	if check.Remedy != "" {
		body += " " + check.Remedy
	}
	return &IdentityProblem{Title: check.Status.Label(), Body: firstNonEmpty(body, "Set up the identity again.")}
}

// identityRowStatus is the row status a lane's identity sets, where it sets
// one: a lane whose vendor offers an identity per seat reads Needs you until
// the seat's own works, and Refused where the vendor refused it; "" leaves
// the row's own status.
func identityRowStatus(la LaneAnswer) LaneStatus {
	id := la.Identity
	if id == nil || !id.Offered || !la.Desired.Enabled {
		return ""
	}
	switch {
	case id.Status == IdentityRefused || id.Holds == HoldsNone:
		return StatusRefused
	case id.Status == IdentityPreparing:
		return StatusPreparing
	case id.Status == IdentityUnchecked:
		return StatusRoleUnchecked
	case !id.Status.works():
		return StatusNeedsYou
	}
	return ""
}

// Check reports what in g breaks the sheet's rules: every text in the one
// vendortext form, and the vendor's page linked on the step that names it,
// with a button under the steps only where no step links a page.
func (g IdentityGuide) Check() error {
	var errs []error
	linked := false
	for i, s := range g.Steps {
		if err := s.Text.Check(); err != nil {
			errs = append(errs, fmt.Errorf("step %d: %w", i+1, err))
		}
		linked = linked || len(s.Text.URLs()) > 0
	}
	for what, text := range map[string]vendortext.Text{"note": g.Note, "back hint": g.Back.Hint, "back text": g.Back.Text} {
		if err := text.Check(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	if g.Make != nil {
		if err := g.Make.Does.Check(); err != nil {
			errs = append(errs, fmt.Errorf("make: %w", err))
		}
	}
	switch {
	case linked && g.Link != nil:
		errs = append(errs, fmt.Errorf("a step links the vendor's page, so the %q button repeats it", g.Link.Label))
	case !linked && g.Link == nil:
		errs = append(errs, errors.New("no step links the vendor's page and no button opens it"))
	}
	return errors.Join(errs...)
}
