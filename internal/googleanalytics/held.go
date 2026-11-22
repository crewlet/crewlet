package googleanalytics

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// prepared is call's request held to the seat's scope and to its target as
// read now, once: its class and its binding are on the same bytes.
func (p *Product) prepared(ctx context.Context, call growth.Call) (googlegrowth.Held, error) {
	req, err := p.built(ctx, call)
	if err != nil {
		return googlegrowth.Held{}, err
	}
	return p.client.Read(ctx, call.Handle, req)
}

// Prepare classes a write against its target as read now and builds what a
// person is asked where it is held.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	h, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	return p.client.Hold(call, h, h.Class()), nil
}

// Card is a held call's decision card as its target reads now.
func (p *Product) Card(ctx context.Context, call growth.Call, binding string) (growth.Card, error) {
	h, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	return p.client.Card(call, h, binding, h.Class()), nil
}

// Apply makes a prepared write once, building it again and, under its
// account's lease, holding it to the seat's scope (an account's summaries
// read included) and reading its target again, sending nothing where it moved
// since binding was taken (googlegrowth's Client.Apply).
func (p *Product) Apply(ctx context.Context, call growth.Call, binding string) (any, error) {
	req, _, err := p.requestOf(call)
	return p.client.Apply(ctx, call, googlegrowth.Write{Req: req, Err: err, Class: p.toolClass(call.Tool),
		Scope: func(ctx context.Context) error { return p.inScope(ctx, call.Handle, req) },
		Cut:   p.cut(call.Handle, req)}, binding)
}

// Denied records that an administrator denied the held call, or that it
// lapsed: its one answer.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	req, _, _ := p.requestOf(call)
	p.client.Denied(ctx, call, req, p.toolClass(call.Tool))
	return nil
}

// toolClass is the class of the tool named, which every record of its call
// names: the class the call was made, or asked and answered, at.
func (p *Product) toolClass(name string) growth.Class {
	tool, _ := p.Catalog().Tool(name)
	return tool.Class
}

// secretGetOp reads a stream's Measurement Protocol secret.
const secretGetOp = "analyticsadmin.properties.dataStreams.measurementProtocolSecrets.get"

// measurementSecret makes a Measurement Protocol call act in the property of
// the stream secret its `api_secret_name` names, which the seat's scope then
// holds like any other. The secret, which the agent never reads, is filled in
// only as the call is sent, so its digest, card and records hold the name.
func (p *Product) measurementSecret(handle string, req *googlegrowth.Request) error {
	name := req.Query.Get("api_secret_name")
	parts := strings.Split(name, "/")
	if req.Query.Has("api_secret") || len(parts) != 6 || parts[0] != "properties" || parts[2] != "dataStreams" ||
		parts[4] != "measurementProtocolSecrets" {
		return refusal(growth.RefusedInvalid, "name the stream's Measurement Protocol secret as query.api_secret_name, "+
			"properties/<id>/dataStreams/<id>/measurementProtocolSecrets/<id>, and never its value. No vendor was reached.")
	}
	op, _ := p.Catalog().Operation(secretGetOp)
	get, err := newRequest(op, map[string]string{"propertiesId": parts[1], "dataStreamsId": parts[3],
		"measurementProtocolSecretsId": parts[5]}, nil, nil)
	if err != nil {
		return err
	}
	req.Account = get.Account
	req.Fill = func(ctx context.Context, q url.Values) (url.Values, error) {
		raw, err := p.client.Send(ctx, handle, get)
		if err != nil {
			return nil, err
		}
		var secret struct {
			Value string `json:"secretValue"`
		}
		if err := json.Unmarshal(raw, &secret); err != nil || secret.Value == "" {
			return nil, &googlegrowth.CallError{Failure: growth.FailureVendorRefused, Err: refusal(
				growth.RefusedNotFound, name+" holds no secret value, so nothing was sent.")}
		}
		q.Del("api_secret_name")
		q.Set("api_secret", secret.Value)
		return q, nil
	}
	return nil
}
