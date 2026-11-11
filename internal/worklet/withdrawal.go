package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// A WITHDRAWN QUESTION SAYS WHY. An asker withdraws its own open question by
// posting a reason in the question's thread and resolving the question with a
// [Withdrawal] naming that reason, and what now covers the question where
// something does (another work item, or a link such as a pull request). The
// person it was put to reads both on the card that closed, rather than seeing
// the question only disappear.

// MaxReplacementURLBytes bounds the link a withdrawal names: the length a
// browser keeps in its address bar, far past any pull request's address.
const MaxReplacementURLBytes = 2048

// Withdrawal is an asker's record of withdrawing their own open question.
type Withdrawal struct {
	// Reason is the comment the asker posted in the question's thread
	// saying why: a reply to the question, by its asker.
	Reason string `json:"reason"`

	// ReplacedBy is what now covers the question, where the asker named it.
	ReplacedBy *Replacement `json:"replaced_by,omitempty"`
}

// Replacement is what covers a withdrawn question: exactly one of a work
// item, by id, or an http or https link.
type Replacement struct {
	Item string `json:"item,omitempty"`
	URL  string `json:"url,omitempty"`
}

// ReplacementURL is raw as a link a withdrawal may name, or why it is not.
func ReplacementURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > MaxReplacementURLBytes {
		return "", invalid("a replacement link is %d bytes and the most is %d", len(raw),
			MaxReplacementURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", invalid("%q is not an http or https link", raw)
	}
	return u.String(), nil
}

// checkWithdrawal holds w to the question it withdraws, inside the resolve's
// own snapshot: the reason is the asker's reply to it, and what replaces it is
// a link or another work item that exists.
func checkWithdrawal(ctx context.Context, tx *sql.Tx, taskID string, question Comment,
	w *Withdrawal) error {

	if question.Ask == "" {
		return invalid("comment %s asks nobody anything, so there is no question to withdraw",
			question.ID)
	}
	held, err := readComment(ctx, tx, taskID, w.Reason)
	if err != nil {
		return err
	}
	reason := held[0]
	if reason.Author != question.Author || reason.ReplyTo == nil || *reason.ReplyTo != question.ID ||
		reason.Removed {
		return invalid("a withdrawal's reason is its asker's reply to the question, and comment "+
			"%s is not %s's reply to %s", w.Reason, question.Author, question.ID)
	}
	r := w.ReplacedBy
	switch {
	case r == nil:
		return nil
	case (r.Item == "") == (r.URL == ""):
		return invalid("a withdrawal's replacement is a work item or a link, exactly one")
	case r.URL != "":
		_, err = ReplacementURL(r.URL)
		return err
	case r.Item == taskID:
		return invalid("a question is not replaced by the item it is on: name the other item, " +
			"or the link, that now covers it")
	}
	ref, err := readTaskRef(ctx, tx, r.Item)
	if err != nil {
		return err
	}
	if ref == nil {
		return fmt.Errorf("%w: %s", ErrNoTask, r.Item)
	}
	return nil
}

// Withdrawn is how a withdrawn question reads: why its asker closed it, and
// what now covers it.
type Withdrawn struct {
	// Reason is the reason comment's words as they stand now, and
	// ReasonComment its id, which opens it in the question's thread.
	Reason        string `json:"reason" untrusted:"fence"`
	ReasonComment string `json:"reason_comment"`

	// ReplacedBy is what covers it, absent where the asker named nothing.
	ReplacedBy *ReplacementRef `json:"replaced_by,omitempty"`
}

// ReplacementRef is a withdrawal's replacement as a reader names it: the work
// item, or the link.
type ReplacementRef struct {
	Item *TaskRef `json:"item,omitempty"`
	URL  string   `json:"url,omitempty"`
}

// readWithdrawn is how the withdrawal of a question on taskID reads, nil for
// none. A reason or item since removed reads as what is left of it.
func readWithdrawn(ctx context.Context, tx *sql.Tx, taskID string, w *Withdrawal) (*Withdrawn, error) {
	if w == nil {
		return nil, nil
	}
	out := &Withdrawn{ReasonComment: w.Reason}
	held, err := readComment(ctx, tx, taskID, w.Reason)
	switch {
	case errors.Is(err, ErrNoComment):
	case err != nil:
		return nil, err
	case !held[0].Removed:
		out.Reason = held[0].Body
	}
	if r := w.ReplacedBy; r != nil {
		switch {
		case r.URL != "":
			out.ReplacedBy = &ReplacementRef{URL: r.URL}
		case r.Item != "":
			ref, err := readTaskRef(ctx, tx, r.Item)
			if err != nil {
				return nil, err
			}
			if ref != nil {
				out.ReplacedBy = &ReplacementRef{Item: ref}
			}
		}
	}
	return out, nil
}
