package googleanalytics

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// prepared builds call's request, holds it to the seat's scope, classes it
// against its target as read now and reads that target. Its request is kept
// on a later failure, for the record of it.
func (p *Product) prepared(ctx context.Context, call growth.Call) (googlegrowth.Held, growth.Class, error) {
	req, _, err := p.requestOf(ctx, call)
	if err != nil {
		return googlegrowth.Held{}, "", err
	}
	if err = p.inScope(ctx, call.Handle, req); err != nil {
		return googlegrowth.Held{Req: req}, "", err
	}
	class, err := p.classOf(ctx, call.Handle, req)
	if err != nil {
		return googlegrowth.Held{Req: req}, "", err
	}
	before, err := p.before(ctx, call.Handle, req)
	if err != nil {
		return googlegrowth.Held{Req: req}, "", err
	}
	return googlegrowth.NewHeld(req, before), class, nil
}

// Prepare classes a write against its target as read now and builds what a
// person is asked where it is held.
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

// Apply makes a prepared write once, reading its target again first and
// sending nothing where it moved since binding was taken.
func (p *Product) Apply(ctx context.Context, call growth.Call, binding string) (any, error) {
	h, class, err := p.prepared(ctx, call)
	if class == "" {
		tool, _ := p.Catalog().Tool(call.Tool)
		class = tool.Class
	}
	return p.client.Apply(ctx, call, h, err, binding, class, p.cut(call.Handle, h.Req))
}

// measurementSecret fills a Measurement Protocol call's api_secret from the
// stream secret its `api_secret_name` names, which the agent never reads:
// the secret's property is held to the seat's scope like any other call.
func (p *Product) measurementSecret(ctx context.Context, handle string, req *googlegrowth.Request) error {
	name := req.Query.Get("api_secret_name")
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "properties" || parts[2] != "dataStreams" || parts[4] != "measurementProtocolSecrets" {
		return refusal(growth.RefusedInvalid, "name the stream's Measurement Protocol secret as query.api_secret_name, "+
			"properties/<id>/dataStreams/<id>/measurementProtocolSecrets/<id>. No vendor was reached.")
	}
	req.Account = "properties/" + parts[1]
	if err := p.inScope(ctx, handle, *req); err != nil {
		return err
	}
	get := growth.Operation{ID: "analyticsadmin.properties.dataStreams.measurementProtocolSecrets.get",
		Method: "GET", Path: adminAPI + "/v1beta/" + name, Class: growth.ClassRead, Permissions: []string{scopeReadonly}}
	raw, err := p.client.Send(ctx, handle, googlegrowth.Request{Op: get, Address: get.Path})
	if err != nil {
		return err
	}
	var secret struct {
		Value string `json:"secretValue"`
	}
	if err := json.Unmarshal(raw, &secret); err != nil || secret.Value == "" {
		return refusal(growth.RefusedNotFound, name+" holds no secret value. No vendor was reached.")
	}
	req.Query.Del("api_secret_name")
	req.Query.Set("api_secret", secret.Value)
	return nil
}
