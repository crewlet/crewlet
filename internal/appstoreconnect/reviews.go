package appstoreconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// replyLimit is the most characters a review reply holds: Apple's App Store
// Connect help for responding to reviews.
const replyLimit = 5970

// excerptRunes bounds a review's excerpt on a person's card.
const excerptRunes = 300

// Review is one customer review as an agent reads it: never its author.
type Review struct {
	ID        string `json:"id"`
	Rating    int    `json:"rating"`
	Title     string `json:"title" untrusted:"name"`
	Body      string `json:"body" untrusted:"fence"`
	Territory string `json:"territory"`
	Created   string `json:"created"`
	// Reply is the team's reply, its state PENDING_PUBLISH or PUBLISHED.
	Reply      string `json:"reply,omitempty" untrusted:"fence"`
	ReplyState string `json:"reply_state,omitempty"`
}

// Reviews is the reviews tool's answer.
type Reviews struct {
	App        string   `json:"app"`
	Reviews    []Review `json:"reviews"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type reviewDoc struct {
	ID         string `json:"id"`
	Attributes struct {
		Rating           int    `json:"rating"`
		Title            string `json:"title"`
		Body             string `json:"body"`
		ReviewerNickname string `json:"reviewerNickname"`
		Territory        string `json:"territory"`
		CreatedDate      string `json:"createdDate"`
	} `json:"attributes"`
	Relationships struct {
		Response struct {
			Data *resourceRef `json:"data"`
		} `json:"response"`
	} `json:"relationships"`
}

type responseDoc struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		ResponseBody string `json:"responseBody"`
		State        string `json:"state"`
	} `json:"attributes"`
}

// reviewsOp is the curated tools' made call on app.
func (p *Product) reviewCall(ctx context.Context, handle, app string, opID string) (made, error) {
	seat, err := p.seat(handle)
	if err != nil {
		return made{}, err
	}
	op, _ := Catalog().Operation(opID)
	m := made{op: op, class: op.Class, query: url.Values{}}
	if m.key, err = p.keyOf(ctx, handle); err != nil {
		return made{}, err
	}
	m.team = m.key.team
	if err := sign(&m, growth.LevelOf(seat.Level)); err != nil {
		return made{}, err
	}
	if err := p.placeApp(ctx, seat, &m, app); err != nil {
		return made{}, err
	}
	return m, nil
}

// reviews lists an app's reviews, newest first.
func (p *Product) reviews(ctx context.Context, call growth.Call) (any, error) {
	app, _ := call.Args["app"].(string)
	m, err := p.reviewCall(ctx, call.Handle, app, opReviews)
	if err != nil {
		return nil, err
	}
	q := url.Values{"sort": {"-createdDate"}, "include": {"response"}, "limit": {"50"}}
	if r, _ := call.Args["rating"].(string); r != "" {
		q.Set("filter[rating]", r)
	}
	if t, _ := call.Args["territory"].(string); t != "" {
		q.Set("filter[territory]", t)
	}
	switch replied, _ := call.Args["replied"].(string); replied {
	case "yes":
		q.Set("exists[publishedResponse]", "true")
	case "no":
		q.Set("exists[publishedResponse]", "false")
	}
	if c, _ := call.Args["cursor"].(string); c != "" {
		q.Set("cursor", c)
	}
	if l, _ := call.Args["limit"].(string); l != "" {
		if n, aerr := strconv.Atoi(l); aerr == nil && n >= 1 && n <= maxListLimit {
			q.Set("limit", l)
		}
	}
	m.path = "/v1/apps/" + url.PathEscape(app) + "/customerReviews"
	resp, err := m.do(ctx, Request{Method: "GET", Path: m.path, Query: q, Read: true})
	if err != nil {
		return nil, said(err)
	}
	p.owners.remember(app, resp.Body)
	var doc struct {
		Data     []reviewDoc   `json:"data"`
		Included []responseDoc `json:"included"`
		Links    struct {
			Next string `json:"next"`
		} `json:"links"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return nil, fmt.Errorf("appstoreconnect: read %s's reviews: %w", app, err)
	}
	responses := map[string]responseDoc{}
	for _, r := range doc.Included {
		responses[r.ID] = r
	}
	out := Reviews{App: app, NextCursor: cursorOf(doc.Links.Next)}
	for _, d := range doc.Data {
		r := Review{ID: d.ID, Rating: d.Attributes.Rating, Title: d.Attributes.Title, Body: d.Attributes.Body,
			Territory: d.Attributes.Territory, Created: d.Attributes.CreatedDate}
		if ref := d.Relationships.Response.Data; ref != nil {
			if resp, ok := responses[ref.ID]; ok {
				r.Reply, r.ReplyState = resp.Attributes.ResponseBody, resp.Attributes.State
			}
		}
		out.Reviews = append(out.Reviews, r)
	}
	p.note(ctx, growth.StepMade, recordOf(call, m))
	return out, nil
}

// reply is one reply of a batch, and the review it answers as it reads now.
type reply struct {
	review   reviewDoc
	current  string
	text     string
	digest   string
	response string
}

// replies reads a reply batch: 1 to 10 replies to reviews of one app, each
// within Apple's limit, each review read now.
func (p *Product) replies(ctx context.Context, call growth.Call) (made, []reply, error) {
	app, _ := call.Args["app"].(string)
	m, err := p.reviewCall(ctx, call.Handle, app, opReply)
	if err != nil {
		return made{}, nil, err
	}
	raw, _ := call.Args["replies"].([]any)
	if len(raw) < 1 || len(raw) > growth.MaxPublicActs {
		return made{}, nil, invalid("`replies` holds 1 to %d replies.", growth.MaxPublicActs)
	}
	seen := map[string]bool{}
	out := make([]reply, 0, len(raw))
	for i, r := range raw {
		obj, _ := r.(map[string]any)
		id, _ := obj["review"].(string)
		text, _ := obj["text"].(string)
		text = strings.TrimSpace(text)
		switch n := utf8.RuneCountInString(text); {
		case !pathValue().MatchString(id) || seen[id]:
			return made{}, nil, invalid("`replies[%d].review` is a review's ID, once.", i)
		case n == 0 || n > replyLimit:
			return made{}, nil, invalid("`replies[%d].text` is 1 to %d characters; it is %d.", i, replyLimit, n)
		}
		seen[id] = true
		found, err := placer{owners: p.owners, key: m.key}.appOf(ctx, "customerReviews", id, []string{app})
		if err != nil || found != app {
			return made{}, nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("review %s is not a review "+
				"of app %s. No call was made.", id, app))
		}
		rv, rerr := p.readReview(ctx, m, id)
		if rerr != nil {
			return made{}, nil, said(rerr)
		}
		rv.text = text
		out = append(out, rv)
	}
	return m, out, nil
}

// readReview reads a review and its current reply.
func (p *Product) readReview(ctx context.Context, m made, id string) (reply, error) {
	resp, err := m.do(ctx, Request{Method: "GET", Path: "/v1/customerReviews/" + url.PathEscape(id),
		Query: url.Values{"include": {"response"}}, Read: true, Reserved: true})
	if err != nil {
		return reply{}, err
	}
	var doc struct {
		Data     reviewDoc     `json:"data"`
		Included []responseDoc `json:"included"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return reply{}, fmt.Errorf("appstoreconnect: read review %s: %w", id, err)
	}
	r := reply{review: doc.Data}
	for _, inc := range doc.Included {
		r.current, r.response = inc.Attributes.ResponseBody, inc.ID
	}
	data, _ := json.Marshal([]any{doc.Data.Attributes.Rating, doc.Data.Attributes.Title, doc.Data.Attributes.Body, r.current})
	r.digest = hexSum(data)
	return r, nil
}

// replyBinding binds a batch: every reply's text and every review's state.
func replyBinding(app string, rs []reply) string {
	parts := make([]any, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, []string{r.review.ID, r.text, r.digest})
	}
	data, _ := json.Marshal([]any{app, parts})
	return hexSum(data)
}

func (p *Product) prepareReplies(ctx context.Context, call growth.Call) (growth.Hold, error) {
	m, rs, err := p.replies(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	if reasonOf(call.Args) == "" {
		return growth.Hold{}, invalid("`reason` is one line saying why, which the administrator reads.")
	}
	question := fmt.Sprintf("May %s reply to %s of app %s?", call.Handle, words.Count(len(rs), "review", "reviews"), m.app)
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks to post these replies, through its own %s key. Allow once posts them all, each exactly "+
		"as shown; Apple publishes a reply within a day.", call.Handle, roleName(m.key.role))
	for i, r := range rs {
		fmt.Fprintf(&b, "\n\n**Review %d** (%d stars, %s): %s", i+1, r.review.Attributes.Rating,
			r.review.Attributes.Territory, words.Inert(textcut.Within(r.review.Attributes.Body, excerptRunes)))
		if r.current != "" {
			b.WriteString("\n\nIt has a reply, which this one replaces.")
		}
		fmt.Fprintf(&b, "\n\n**Reply %d** (%d of %d characters)\n\n%s", i+1, utf8.RuneCountInString(r.text), replyLimit,
			words.CodeBlock(textcut.Within(r.text, 1200)))
	}
	b.WriteString("\n\nThe call's page shows every reply in full.\n\n**Reason given**\n\n" + words.Inert(reasonOf(call.Args)))
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.review.ID)
	}
	ask := recordOf(call, m)
	ask.Operation = replyBinding(m.app, rs)
	changes := make([]growth.CardChange, 0, len(rs))
	for _, r := range rs {
		changes = append(changes, growth.CardChange{Field: "responseBody", Label: "Reply to review " + r.review.ID,
			Before: r.current, After: r.text})
	}
	ask.Changes = recordChanges(changes, true)
	return growth.Hold{Class: growth.ClassPublic, Binding: replyBinding(m.app, rs), Question: question,
		Context: textcut.Within(b.String(), sandbox.MaxAskContextBytes-1024),
		Subject: delegated.Subject(delegated.KindPublicPost, append([]string{m.app}, ids...)...), Ask: ask}, nil
}

func (p *Product) replyCard(ctx context.Context, call growth.Call, approved string) (growth.Card, error) {
	m, rs, err := p.replies(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	card := growth.Card{Act: delegated.KindPublicPost, Product: Kind, Tool: call.Tool,
		Account: growth.Account{Kind: Kind, Connection: m.team.id, ID: m.app, State: growth.AccountServing},
		Reason:  reasonOf(call.Args), Moved: !growth.BindingHolds(approved, replyBinding(m.app, rs))}
	for _, r := range rs {
		card.Acts = append(card.Acts, growth.CardAct{Target: "review " + r.review.ID, Summary: "reply to a review",
			Text: r.text, TextLimit: replyLimit, TargetRating: r.review.Attributes.Rating,
			TargetAuthor:  r.review.Attributes.ReviewerNickname,
			TargetExcerpt: textcut.Within(r.review.Attributes.Body, excerptRunes),
			Changes:       []growth.CardChange{{Field: "responseBody", Label: "Reply", Before: r.current, After: r.text}}})
	}
	return card, nil
}

// Posted is the reply tool's answer: each reply's review and what Apple said.
type Posted struct {
	App     string        `json:"app"`
	Replies []PostedReply `json:"replies"`
}

// PostedReply is one reply's outcome.
type PostedReply struct {
	Review string `json:"review"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty" untrusted:"fence"`
}

func (p *Product) applyReplies(ctx context.Context, call growth.Call, approved string) (any, error) {
	m, rs, err := p.replies(ctx, call)
	rec := p.heldRecord(call, m, err == nil)
	if err == nil {
		rec.Operation = replyBinding(m.app, rs)
	}
	if aerr := p.allow(ctx, rec, call.Approver); aerr != nil {
		return nil, aerr
	}
	if err != nil {
		return nil, p.failEarly(ctx, true, rec, err)
	}
	release, err := p.hold(ctx, leaseOf(m))
	if err != nil {
		return nil, p.failEarly(ctx, true, rec, err)
	}
	defer release()
	if !growth.BindingHolds(approved, replyBinding(m.app, rs)) {
		p.note(ctx, growth.StepVoid, rec)
		return nil, growth.ErrMoved
	}
	act, cancel := context.WithTimeout(ctx, p.applyBound)
	defer cancel()
	out := Posted{App: m.app}
	failed, unknown := 0, false
	var first growth.Failure
	for _, r := range rs {
		body := map[string]any{"data": map[string]any{"type": "customerReviewResponses",
			"attributes":    map[string]any{"responseBody": r.text},
			"relationships": map[string]any{"review": map[string]any{"data": map[string]any{"type": "customerReviews", "id": r.review.ID}}}}}
		_, err := m.do(act, Request{Method: "POST", Path: "/v1/customerReviewResponses", Body: body,
			Reserved: true})
		pr := PostedReply{Review: r.review.ID, Status: "posted"}
		switch f := writeFailure(err); {
		case f == growth.FailureUnknownOutcome:
			unknown = true
			pr.Status, pr.Error = "unknown", "the reply was cut before Apple answered: read the review to see "+
				"whether it was posted."
		case err != nil:
			failed++
			if first == "" {
				first = f
			}
			pr.Status, pr.Error = "failed", said(err).Error()
		}
		out.Replies = append(out.Replies, pr)
	}
	switch {
	case unknown:
		rec.Failure = growth.FailureUnknownOutcome
		p.note(ctx, growth.StepFailed, rec)
	case failed == len(rs):
		// EVERY REPLY FAILED, the act as its first reply did: Apple's refusal,
		// or a request never sent, which is not Apple's.
		rec.Failure = first
		p.note(ctx, growth.StepFailed, rec)
	default:
		p.note(ctx, growth.StepApplied, rec)
	}
	return out, nil
}
