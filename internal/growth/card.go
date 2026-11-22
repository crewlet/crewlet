package growth

import (
	"time"

	"github.com/crewlet/crewlet/internal/delegated"
)

// Card is what an administrator reads before answering a held growth act:
// read live when the held call is opened (`GET /mcp-approvals/{id}`), so it
// shows the target as it is now and says whether it moved since the ask.
type Card struct {
	// Act is the decision's kind: ad_spend, public_post or account_admin.
	Act delegated.Kind `json:"act"`
	// Product is the integration kind; Tool the tool the agent called.
	Product string `json:"product"`
	Tool    string `json:"tool"`
	// Account is the vendor account the act changes.
	Account Account `json:"account"`
	// Acts are the changes, one for spend and admin, 1 to 10 for public,
	// allowed or denied whole.
	Acts []CardAct `json:"acts"`
	// Money is the spend guard's figures, nil for an act that spends nothing.
	Money *CardMoney `json:"money"`
	// Findings are the vendor's own findings from validating the act.
	Findings []string `json:"findings"`
	// Reason is the agent's reason, as it gave it.
	Reason string `json:"reason"`
	// LapsesAt is when the ask lapses unanswered.
	LapsesAt time.Time `json:"lapses_at"`
	// Moved is true when the target changed since the ask: Allow once would
	// run nothing, and the agent is told to ask again.
	Moved bool `json:"moved"`
	// OtherOpenAsks counts other open asks that change the same target.
	OtherOpenAsks int `json:"other_open_asks"`
}

// CardAct is one change of a held act, shown whole: a person sees exactly
// what is sent.
type CardAct struct {
	// Target names what the act changes, as its vendor names it.
	Target string `json:"target"`
	// Summary is the change in one line.
	Summary string `json:"summary"`
	// Changes are the target's changed fields, before and after.
	Changes []CardChange `json:"changes"`
	// Danger marks an act that cannot be undone (a delete, a revoke), which
	// the card asks to be confirmed.
	Danger bool `json:"danger"`
	// Text is what is published, in full, never cut; TextLimit is the
	// vendor's limit in characters, 0 for none.
	Text      string `json:"text,omitempty"`
	TextLimit int    `json:"text_limit"`
	// TargetExcerpt, TargetRating and TargetAuthor describe what a reply
	// answers: an excerpt of the review, its stars and its author's name,
	// shown on the card alone.
	TargetExcerpt string `json:"target_excerpt,omitempty"`
	TargetRating  int    `json:"target_rating"`
	TargetAuthor  string `json:"target_author,omitempty"`
}

// CardChange is one field an act changes. Money values are integer micros of
// the account's currency, written as a decimal string; every other value is
// the vendor's own text.
type CardChange struct {
	Field string `json:"field"`
	// Label is the field in the product's words ("Daily budget").
	Label string `json:"label"`
	// Before is "" for a field the target does not hold yet.
	Before string `json:"before"`
	After  string `json:"after"`
	Money  bool   `json:"money"`
}

// CardMoney is a spend act's figures in micros of Currency, as JSON strings
// so an int64 stays exact in a browser.
type CardMoney struct {
	Currency           string `json:"currency"`
	DeltaMicros        int64  `json:"delta_micros,string"`
	DailyMicros        int64  `json:"daily_micros,string"`
	DailyAfterMicros   int64  `json:"daily_after_micros,string"`
	PerChangeCapMicros int64  `json:"per_change_cap_micros,string"`
	DailyCapMicros     int64  `json:"daily_cap_micros,string"`
	// Overdelivery is the vendor's own rule on spending past a daily budget,
	// in one sentence.
	Overdelivery string `json:"overdelivery,omitempty"`
}

// MaxPublicActs is the most acts one public ask carries: each ask costs a
// fresh step-up, and a busy app gets dozens of reviews a day.
const MaxPublicActs = 10
