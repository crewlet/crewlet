package searchconsole

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// prepared builds call's request, holds it to the seat's sites and Google's
// terms ([Product.checked]), then reads its target. Its request is kept on a
// later failure, for the record of it. No Search Console method's class
// depends on its arguments, so a call comes to its tool's class.
func (p *Product) prepared(ctx context.Context, call growth.Call) (googlegrowth.Held, growth.Class, error) {
	req, class, err := p.requestOf(ctx, call)
	if err != nil {
		return googlegrowth.Held{}, "", err
	}
	if err = p.checked(ctx, call.Handle, req); err != nil {
		return googlegrowth.Held{Req: req}, "", err
	}
	h, err := p.client.Read(ctx, call.Handle, req)
	return h, class, err
}

// checked holds req to the seat's sites and, for an Indexing API
// notification, to Google's terms as its page reads now.
func (p *Product) checked(ctx context.Context, handle string, req googlegrowth.Request) error {
	if err := p.inScope(handle, req); err != nil {
		return err
	}
	if req.Op.ID != "indexing.urlNotifications.publish" {
		return nil
	}
	page, _ := req.Body["url"].(string)
	kind, _ := req.Body["type"].(string)
	if kind != notifyUpdated && kind != notifyDeleted {
		return refusal(growth.RefusedInvalid, "an Indexing API notification's type is URL_UPDATED or "+
			"URL_DELETED. No vendor was reached.")
	}
	return pageAllows(ctx, p.pages, page, kind)
}

// Prepare reads a write's target live and builds what a person is asked
// where it is held.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	h, class, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	return p.client.Hold(call, h, class), nil
}

// Card is a held call's decision card as its target reads now.
func (p *Product) Card(ctx context.Context, call growth.Call, binding string) (growth.Card, error) {
	h, class, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	return p.client.Card(call, h, binding, class), nil
}

// Apply makes a prepared write once, building it again and, under its site's
// lease (googlegrowth's Client.Apply), holding it to the seat's sites and
// Google's terms and reading its target again, sending nothing where it moved
// since binding was taken.
func (p *Product) Apply(ctx context.Context, call growth.Call, binding string) (any, error) {
	req, _, err := p.requestOf(ctx, call)
	return p.client.Apply(ctx, call, googlegrowth.Write{Req: req, Err: err, Class: p.toolClass(call.Tool),
		Scope: func(ctx context.Context) error { return p.checked(ctx, call.Handle, req) },
		Cut:   p.cut(call.Handle, req)}, binding)
}

// unread are the writes whose target holds no state to bind to: adding a
// property to the identity's list, which Google answers 403 for before it is
// there, and a notification, whose page was just read instead.
var unread = []string{"webmasters.sites.add", "indexing.urlNotifications.publish"}

// before is the target's live state, the JSON Google answers for it, or none:
// for a create, an unread write, and a sitemap not submitted yet, which its
// PUT makes (googlegrowth's Client.BeforeRead, which reads every target here).
func (p *Product) before(ctx context.Context, handle string, req googlegrowth.Request) ([]byte, bool, error) {
	address := req.Resource()
	if address == "" || slices.Contains(unread, req.Op.ID) {
		return nil, true, nil
	}
	scope := scopeReadonly
	if strings.HasPrefix(req.Op.ID, "siteVerification.") {
		scope = scopeVerify
	}
	get := growth.Operation{ID: req.Op.ID + ".before", Method: "GET", Path: address, Class: growth.ClassRead,
		Roles: req.Op.Roles, Permissions: []string{scope}}
	raw, err := p.client.Send(ctx, handle, googlegrowth.Request{Op: get, Address: address, Account: req.Account})
	if api, ok := errors.AsType[*googleauth.APIError](err); ok && api.Status == http.StatusNotFound {
		return nil, true, nil
	}
	return raw, err == nil, err
}
