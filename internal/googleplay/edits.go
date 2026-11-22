package googleplay

import (
	"context"
	"errors"
	"log/slog"
	"net/url"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// The edit session's own methods (package doc, "Edits are the engine's").
const (
	opEditInsert   = "androidpublisher.edits.insert"
	opEditDelete   = "androidpublisher.edits.delete"
	opEditValidate = "androidpublisher.edits.validate"
	opEditCommit   = "androidpublisher.edits.commit"
)

// errorIfInReview is the commit's behaviour unless the approved call names
// another: a submission a person sent for review is never cancelled by a
// commit nobody said would cancel it.
const errorIfInReview = "ERROR_IF_IN_REVIEW"

// sessionRequest is one of the edit session's own requests on app, for the
// edit id where it names one.
func (p *Product) sessionRequest(id, app, editID string, query url.Values) (googlegrowth.Request, error) {
	op, _ := p.Catalog().Operation(id)
	req, err := newRequest(op, map[string]string{"packageName": app}, nil, nil)
	if err != nil {
		return req, err
	}
	if query != nil {
		req.Query = query
	}
	return inEdit(req, editID), nil
}

// openEdit opens an edit of app as handle's identity.
func (p *Product) openEdit(ctx context.Context, handle, app string) (string, error) {
	req, err := p.sessionRequest(opEditInsert, app, "", nil)
	if err != nil {
		return "", err
	}
	raw, err := p.client.Send(ctx, handle, req)
	if err != nil {
		return "", err
	}
	var edit struct {
		ID string `json:"id"`
	}
	if err := decode(raw, "an edit", &edit); err != nil || edit.ID == "" {
		return "", refusal(growth.RefusedUnavailable, "Google Play opened no edit of "+app+". Nothing was changed.")
	}
	return edit.ID, nil
}

// discardEdit deletes an edit the engine opened and will not commit: a
// failure to is logged, since Google lets an unused edit lapse anyway.
func (p *Product) discardEdit(ctx context.Context, handle, app, id string) {
	req, err := p.sessionRequest(opEditDelete, app, id, nil)
	if err == nil {
		_, err = p.client.Send(context.WithoutCancel(ctx), handle, req)
	}
	if err != nil {
		slog.WarnContext(ctx, "google_play_edit_kept", "app", app, "err", err.Error(),
			"detail", "Google lets it lapse on its own")
	}
}

// inThrowawayEdit runs read in an edit of app the engine opens and discards.
func (p *Product) inThrowawayEdit(ctx context.Context, handle, app string, read func(editID string) error) error {
	id, err := p.openEdit(ctx, handle, app)
	if err != nil {
		return err
	}
	defer p.discardEdit(ctx, handle, app, id)
	return read(id)
}

// readStaged reads req, a method inside an edit, in a throwaway edit.
func (p *Product) readStaged(ctx context.Context, call growth.Call, req googlegrowth.Request) (any, error) {
	var out googlegrowth.Answer
	err := p.inThrowawayEdit(ctx, call.Handle, req.Account, func(id string) error {
		var sendErr error
		out, sendErr = p.client.SendAndRead(ctx, call.Handle, inEdit(req, id), nil)
		return sendErr
	})
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DryRun is a dry run's answer: Google's validation of the staged changes,
// whose edit was discarded.
type DryRun struct {
	Valid  bool   `json:"valid"`
	Answer string `json:"answer" untrusted:"fence"`
}

// dryRun stages a call's steps in an edit, validates it and discards it:
// nothing is committed, so it reads, and an upload is refused (its bytes
// would reach Google).
func (p *Product) dryRun(ctx context.Context, call growth.Call, req googlegrowth.Request) (any, error) {
	raw, _ := call.Args["steps"].([]any)
	if len(raw) == 0 || len(raw) > maxSteps {
		return nil, refusal(growth.RefusedInvalid, "a dry run names 1 to 20 `steps` to stage and validate. No "+
			"vendor was reached.")
	}
	var reqs []googlegrowth.Request
	for _, r := range raw {
		s, err := p.stepRequest(objectArg(r))
		if err != nil {
			return nil, err
		}
		if s.Account != req.Account {
			return nil, refusal(growth.RefusedInvalid, "every step stages into the dry run's own app. No vendor "+
				"was reached.")
		}
		if uploads(s.Op) {
			return nil, refusal(growth.RefusedInvalid, s.Op.ID+" uploads a file, which a dry run never sends. "+
				"No vendor was reached.")
		}
		reqs = append(reqs, s)
	}
	var out DryRun
	err := p.inThrowawayEdit(ctx, call.Handle, req.Account, func(id string) error {
		for _, s := range reqs {
			if _, stageErr := p.client.Send(ctx, call.Handle, inEdit(s, id)); stageErr != nil {
				return invalidEdit{stageErr}
			}
		}
		answer, validateErr := p.client.SendAndRead(ctx, call.Handle, inEdit(req, id), nil)
		if validateErr != nil {
			return invalidEdit{validateErr}
		}
		out.Valid, out.Answer = true, answer.Answer
		return nil
	})
	// GOOGLE REFUSING WHAT WAS STAGED IS THE DRY RUN'S ANSWER, not its failure.
	if invalid, ok := errors.AsType[invalidEdit](err); ok {
		out.Answer, err = invalid.Error(), nil
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// invalidEdit is Google refusing a dry run's staged change or its validation.
type invalidEdit struct{ error }
