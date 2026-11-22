package googleplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// Limits Google Play sets on what is published, checked before a person is
// asked (developers.google.com/android-publisher/reply-to-reviews; Play
// Console's listing limits).
const (
	replyLimit = 350
	titleLimit = 30
	shortLimit = 80
	fullLimit  = 4000
	notesLimit = 500
)

// excerptRunes bounds a review's excerpt on a person's card.
const excerptRunes = 300

const (
	opReviewsList  = "androidpublisher.reviews.list"
	opReviewsGet   = "androidpublisher.reviews.get"
	opReviewsReply = "androidpublisher.reviews.reply"
)

// Review is one review as an agent reads it.
type Review struct {
	ID         string `json:"id"`
	Author     string `json:"author" untrusted:"name"`
	Rating     int    `json:"rating"`
	Text       string `json:"text" untrusted:"fence"`
	Original   string `json:"original_text,omitempty" untrusted:"fence"`
	Language   string `json:"language,omitempty"`
	Device     string `json:"device,omitempty" untrusted:"name"`
	AppVersion string `json:"app_version,omitempty" untrusted:"name"`
	Modified   string `json:"modified,omitempty"`
	Reply      string `json:"reply,omitempty" untrusted:"fence"`
	Replied    string `json:"replied,omitempty"`
}

// Reviews is the reviews tool's answer.
type Reviews struct {
	App           string   `json:"app"`
	Reviews       []Review `json:"reviews"`
	NextPageToken string   `json:"next_page_token,omitempty"`
	Note          string   `json:"note"`
}

// reviewDoc is a review as Google answers it.
type reviewDoc struct {
	ReviewID   string `json:"reviewId"`
	AuthorName string `json:"authorName"`
	Comments   []struct {
		UserComment *struct {
			Text         string `json:"text"`
			OriginalText string `json:"originalText"`
			StarRating   int    `json:"starRating"`
			Language     string `json:"reviewerLanguage"`
			Device       string `json:"device"`
			AppVersion   string `json:"appVersionName"`
			LastModified struct {
				Seconds string `json:"seconds"`
			} `json:"lastModified"`
		} `json:"userComment"`
		DeveloperComment *struct {
			Text         string `json:"text"`
			LastModified struct {
				Seconds string `json:"seconds"`
			} `json:"lastModified"`
		} `json:"developerComment"`
	} `json:"comments"`
}

// review is d as an agent reads it.
func (d reviewDoc) review() Review {
	r := Review{ID: d.ReviewID, Author: d.AuthorName}
	for _, c := range d.Comments {
		if u := c.UserComment; u != nil {
			r.Rating, r.Text, r.Original, r.Language = u.StarRating, u.Text, u.OriginalText, u.Language
			r.Device, r.AppVersion, r.Modified = u.Device, u.AppVersion, u.LastModified.Seconds
		}
		if dc := c.DeveloperComment; dc != nil {
			r.Reply, r.Replied = dc.Text, dc.LastModified.Seconds
		}
	}
	return r
}

// listReviews lists an app's reviews, a page at a time.
func (p *Product) listReviews(ctx context.Context, call growth.Call) (any, error) {
	app, _ := call.Args["app"].(string)
	op, _ := p.Catalog().Operation(opReviewsList)
	q := map[string]string{}
	if v, _ := call.Args["translation_language"].(string); v != "" {
		q["translationLanguage"] = v
	}
	if v, ok := call.Args["max_results"].(float64); ok && v >= 1 && v <= 100 {
		q["maxResults"] = strconv.Itoa(int(v))
	}
	if v, _ := call.Args["page_token"].(string); v != "" {
		q["token"] = v
	}
	req, err := newRequest(op, map[string]string{"packageName": app}, q, nil)
	if err != nil {
		return nil, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return nil, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Reviews         []reviewDoc `json:"reviews"`
		TokenPagination struct {
			Next string `json:"nextPageToken"`
		} `json:"tokenPagination"`
	}
	if err := decode(raw, app+"'s reviews", &doc); err != nil {
		return nil, err
	}
	out := Reviews{App: app, NextPageToken: doc.TokenPagination.Next, Reviews: []Review{},
		Note: "Google Play's API answers only reviews written or changed in the last 7 days; the monthly " +
			"reviews report (google_play_read_report, reviews/) holds older ones."}
	for _, d := range doc.Reviews {
		out.Reviews = append(out.Reviews, d.review())
	}
	return out, nil
}

// reply is one reply of a batch and the review it answers, as it reads now.
type reply struct {
	review reviewDoc
	text   string
	digest string
}

// replyBatch is a reply batch: 1 to 10 replies to reviews of one app.
type replyBatch struct {
	app     string
	reason  string
	replies []reply
}

// replies reads a reply batch, each within Google's limit, each review read
// now (which also proves it is a review of the app).
func (p *Product) replies(ctx context.Context, call growth.Call) (replyBatch, error) {
	app, _ := call.Args["app"].(string)
	reason, _ := call.Args["reason"].(string)
	m := replyBatch{app: app, reason: strings.TrimSpace(reason)}
	op, _ := p.Catalog().Operation(opReviewsGet)
	probe, err := newRequest(op, map[string]string{"packageName": app, "reviewId": "probe"}, nil, nil)
	if err != nil {
		return m, err
	}
	if err := p.inScope(call.Handle, probe); err != nil {
		return m, err
	}
	raw, _ := call.Args["replies"].([]any)
	if len(raw) < 1 || len(raw) > growth.MaxPublicActs {
		return m, refusal(growth.RefusedInvalid, fmt.Sprintf("`replies` holds 1 to %d replies. No vendor was "+
			"reached.", growth.MaxPublicActs))
	}
	seen := map[string]bool{}
	for i, r := range raw {
		obj, _ := r.(map[string]any)
		id, _ := obj["review"].(string)
		text, _ := obj["text"].(string)
		text = strings.TrimSpace(text)
		switch n := utf8.RuneCountInString(text); {
		case !googlegrowth.IDParam("reviewId", id) || seen[id]:
			return m, refusal(growth.RefusedInvalid, fmt.Sprintf("`replies[%d].review` is a review's ID, once. "+
				"No vendor was reached.", i))
		case n == 0 || n > replyLimit:
			return m, refusal(growth.RefusedInvalid, fmt.Sprintf("`replies[%d].text` is 1 to %d characters; it "+
				"is %d. No vendor was reached.", i, replyLimit, n))
		}
		seen[id] = true
		get, err := newRequest(op, map[string]string{"packageName": app, "reviewId": id}, nil, nil)
		if err != nil {
			return m, err
		}
		body, err := p.client.Send(ctx, call.Handle, get)
		if err != nil {
			return m, err
		}
		var doc reviewDoc
		if err := decode(body, "review "+id, &doc); err != nil {
			return m, err
		}
		state, _ := json.Marshal(doc)
		sum := sha256.Sum256(state)
		m.replies = append(m.replies, reply{review: doc, text: text, digest: hex.EncodeToString(sum[:])})
	}
	return m, nil
}

// replyBinding binds a batch: every reply's text and every review's state.
func replyBinding(m replyBatch) string {
	parts := make([]any, 0, len(m.replies))
	for _, r := range m.replies {
		parts = append(parts, []string{r.review.ReviewID, r.text, r.digest})
	}
	data, _ := json.Marshal([]any{m.app, parts})
	return hexSum(data)
}

// replyRecord is a batch's act as the access log keeps it: each reply as its
// digest and length.
func (p *Product) replyRecord(call growth.Call, m replyBatch) growth.Record {
	op, _ := p.Catalog().Operation(opReviewsReply)
	r := p.client.Record(call, googlegrowth.Request{Op: op, Account: m.app,
		Address: publisherAPI + "/androidpublisher/v3/applications/" + m.app + "/reviews"}, growth.ClassPublic, nil)
	r.Operation = replyBinding(m)
	for _, rp := range m.replies {
		r.Changes = append(r.Changes, googlegrowth.RecordChange(growth.Change{Field: "replyText",
			Before: nonEmpty(rp.review.review().Reply), After: rp.text}))
	}
	return r
}

// nonEmpty is s, nil where it is empty.
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (p *Product) prepareReplies(ctx context.Context, call growth.Call) (growth.Hold, error) {
	m, err := p.replies(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	if m.reason == "" {
		return growth.Hold{}, refusal(growth.RefusedInvalid, "`reason` is one line saying why, which the "+
			"administrator reads. No vendor was reached.")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks to post these replies on Google Play's store page for %s. Allow once posts them "+
		"all, each exactly as shown; a reviewer is told of a first reply.", call.Handle, m.app)
	ids := []string{m.app}
	for i, r := range m.replies {
		rv := r.review.review()
		fmt.Fprintf(&b, "\n\n**Review %d** (%d stars, %s): %s", i+1, rv.Rating, rv.Language,
			words.Inert(textcut.Within(rv.Text, excerptRunes)))
		if rv.Reply != "" {
			b.WriteString("\n\nIt has a reply, which this one replaces.")
		}
		fmt.Fprintf(&b, "\n\n**Reply %d** (%d of %d characters)\n\n%s", i+1, utf8.RuneCountInString(r.text),
			replyLimit, words.CodeBlock(r.text))
		ids = append(ids, rv.ID)
	}
	b.WriteString("\n\n**Reason given**\n\n" + words.Inert(m.reason))
	return growth.Hold{Class: growth.ClassPublic, Binding: replyBinding(m),
		Question: fmt.Sprintf("May %s reply to %s of Google Play app %s?", call.Handle,
			words.Count(len(m.replies), "review", "reviews"), m.app),
		Context: textcut.Within(b.String(), sandbox.MaxAskContextBytes-1024),
		Subject: delegated.Subject(delegated.KindPublicPost, ids...), Ask: p.replyRecord(call, m)}, nil
}

func (p *Product) replyCard(ctx context.Context, call growth.Call, approved string) (growth.Card, error) {
	m, err := p.replies(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	card := growth.Card{Act: delegated.KindPublicPost, Product: string(Kind), Tool: call.Tool, Reason: m.reason,
		Account: growth.Account{Kind: string(Kind), Connection: googlegrowth.Connection, ID: m.app,
			State: growth.AccountServing},
		Moved: !growth.BindingHolds(approved, replyBinding(m))}
	for _, r := range m.replies {
		rv := r.review.review()
		card.Acts = append(card.Acts, growth.CardAct{Target: "review " + rv.ID, Summary: "reply to a review",
			Text: r.text, TextLimit: replyLimit, TargetRating: rv.Rating, TargetAuthor: rv.Author,
			TargetExcerpt: textcut.Within(rv.Text, excerptRunes),
			Changes:       []growth.CardChange{{Field: "replyText", Label: "Reply", Before: rv.Reply, After: r.text}}})
	}
	return card, nil
}

// Posted is the reply tool's answer: each reply's review and how it went.
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
	if call.HeldCall == "" {
		return nil, refusal(growth.RefusedForbidden, "replies are posted only once an administrator allows them. "+
			"No reply was posted.")
	}
	m, err := p.replies(ctx, call)
	rec := p.replyRecord(call, m)
	if aerr := p.client.Allow(ctx, rec, call.Approver); aerr != nil {
		return nil, aerr
	}
	if err != nil {
		return nil, p.failEarly(ctx, true, rec, err)
	}
	release, err := p.hold(ctx, "write/"+string(Kind)+"/"+m.app)
	if err != nil {
		return nil, p.failEarly(ctx, true, rec, err)
	}
	defer release()
	// THE REVIEWS ARE READ AGAIN UNDER THE LEASE, so two batches answering one
	// review cannot both pass on what each read before the other posted.
	if m, err = p.replies(ctx, call); err != nil {
		return nil, p.failEarly(ctx, true, rec, err)
	}
	if !growth.BindingHolds(approved, replyBinding(m)) {
		p.client.Note(ctx, growth.StepVoid, rec)
		return nil, growth.ErrMoved
	}
	bound, cancel := context.WithTimeout(ctx, growth.ApplyBound)
	defer cancel()
	op, _ := p.Catalog().Operation(opReviewsReply)
	out := Posted{App: m.app}
	failed, unknown := 0, false
	for _, r := range m.replies {
		req, err := newRequest(op, map[string]string{"packageName": m.app, "reviewId": r.review.ReviewID}, nil,
			map[string]any{"replyText": r.text})
		if err == nil {
			_, err = p.client.Send(bound, call.Handle, req)
		}
		pr := PostedReply{Review: r.review.ReviewID, Status: "posted"}
		switch {
		case err != nil && googlegrowth.FailureOf(err) == growth.FailureTimedOut:
			unknown = true
			pr.Status, pr.Error = "unknown", "the reply was cut before Google answered: read the review to see "+
				"whether it was posted."
		case err != nil:
			failed++
			pr.Status, pr.Error = "failed", err.Error()
		}
		out.Replies = append(out.Replies, pr)
	}
	switch {
	case unknown:
		rec.Failure = growth.FailureUnknownOutcome
		p.client.Note(ctx, growth.StepFailed, rec)
	case failed == len(m.replies):
		rec.Failure = growth.FailureVendorRefused
		p.client.Note(ctx, growth.StepFailed, rec)
	default:
		p.client.Note(ctx, growth.StepApplied, rec)
	}
	return out, nil
}

// hexSum is data's SHA-256, in hex.
func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
