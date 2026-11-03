package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// THE DATASTORE'S READ QUESTIONS: an artifact's documents, one document, a
// query over its field index, its history, its schema (with the store's
// footprint and state digest) and the caller's own per-viewer state, each
// DECIDED PER ARTIFACT through the one admission the datastore's write routes
// take ([artifactsapi.Gate], the artifacts' own), and each shaped by the
// collection's read rule for the reader the admission names, history included.
//
// EACH IS A READ OF THE ARTIFACT'S CONTENT ([artifactsapi.Gate.Content]): a
// native artifact's datastore is what it holds (D03), so an artifact in the
// trash answers none of them, and an administrator only the admin path admits
// is recorded as reading it before a row is answered (D09).

// ArtifactDataReader is the datastore's read side these questions ask: the
// estate router's, so a node whose own copy is out of service answers from a
// peer's.
type ArtifactDataReader interface {
	Docs(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
		since int64, cursor string, limit int) (artifactdata.DocPage, error)
	Doc(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection, doc string) (
		artifactdata.Doc, error)
	Query(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
		q query.Query) (artifactdata.DocPage, error)
	History(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, filter artifactdata.HistoryFilter,
		cursor string, limit int) (artifactdata.HistoryPage, error)
	Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error)
	Store(ctx context.Context, id artifacts.ID) (artifactdata.StoreInfo, error)
	Usage(ctx context.Context) (artifactdata.Usage, error)
	State(ctx context.Context, id artifacts.ID, rd artifactdata.Reading) ([]artifactdata.StateEntry, error)
}

// ArtifactDataSource is what the datastore's questions read: the admission and
// the reads.
type ArtifactDataSource struct {
	Gate  artifactsapi.Gate
	Reads ArtifactDataReader
}

// artifactData is the source as the questions answer from it, on the one clock
// every answer here reads ([Sources.clock]).
type artifactData struct {
	ArtifactDataSource
	now func() time.Time
}

// registerArtifactData registers the datastore's questions.
func registerArtifactData(r *Registry, src ArtifactDataSource, now func() time.Time) {
	src.Gate.Now = now
	s := artifactData{ArtifactDataSource: src, now: now}
	r.Register("artifact_data", DecidedBy(authz.ActionArtifactDataRead), s.docs)
	r.Register("artifact_data_doc", DecidedBy(authz.ActionArtifactDataRead), s.doc)
	r.Register("artifact_data_query", DecidedBy(authz.ActionArtifactDataQuery), s.query)
	r.Register("artifact_data_history", DecidedBy(authz.ActionArtifactDataRead), s.history)
	r.Register("artifact_data_schema", DecidedBy(authz.ActionArtifactDataRead), s.schema)
	r.Register("artifact_data_state", DecidedBy(authz.ActionArtifactDataRead), s.state)
	// THE COMPANY'S DATASTORE FOOTPRINT against its plan quota: the
	// deployment's own machinery, so the usage verb's (admin's).
	r.Register("artifact_data_usage", DecidedBy(authz.ActionArtifactUsage), s.usage)
}

// admit decides one question's verb on the artifact its `id` names, as a read
// of its content.
func (s artifactData) admit(ctx context.Context, what string, action authz.Action, p Params) (
	artifacts.ID, artifactsapi.Admission, error) {

	id, err := artifacts.ParseID(p.String("id"))
	if err != nil {
		return artifacts.ID{}, artifactsapi.Admission{}, fmt.Errorf("%w: no artifact %q",
			ErrNotFound, p.String("id"))
	}
	principal, err := caller(ctx, what)
	if err != nil {
		return id, artifactsapi.Admission{}, err
	}
	a, err := s.Gate.Content(ctx, principal, action, id, 0)
	return id, a, artifactFailure(what, err)
}

// caller is the principal a question that decides by its caller is answered
// for: an artifact's, its datastore's or the code host's.
//
// A CALLER THIS NODE COULD NOT CHECK IS UNAVAILABLE, never a stranger: decided
// as the zero principal it would be refused as one, and a person holding a
// good credential would be told an artifact shared with them is not there for
// as long as the identity estate is unreachable. The registry answers such a
// context before a question runs; this holds the same rule for any path that
// reaches an answer without it.
func caller(ctx context.Context, what string) (iam.Principal, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return iam.Principal{}, unresolved(ctx, what)
	}
	return principal, nil
}

// dataFailure is a datastore read's failure in this surface's vocabulary.
func dataFailure(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifactdata.ErrNotFound), errors.Is(err, artifactdata.ErrGone):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, artifactdata.ErrInvalid), errors.Is(err, artifactdata.ErrBadCursor),
		errors.Is(err, query.ErrInvalid), errors.Is(err, query.ErrNotIndexed):
		return refusedBy(err, artifactdata.Sentence(err))
	}
	return err
}

// docs answers one page of a collection, or a catch-up with `since=`.
func (s artifactData) docs(ctx context.Context, p Params) (any, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	since, ok := p.WholeInt("since")
	if p.Has("since") && (!ok || since < 0) {
		return nil, fmt.Errorf("%w: the since parameter is a version a read returned", ErrBadParams)
	}
	id, a, err := s.admit(ctx, "artifact_data", authz.ActionArtifactDataRead, p)
	if err != nil {
		return nil, err
	}
	page, err := s.Reads.Docs(ctx, id, artifactdataapi.Reading(a, fresh), p.String("collection"), since,
		p.String("cursor"), p.Int("limit", 0))
	return page, dataFailure(err)
}

// doc answers one document.
func (s artifactData) doc(ctx context.Context, p Params) (any, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	id, a, err := s.admit(ctx, "artifact_data_doc", authz.ActionArtifactDataRead, p)
	if err != nil {
		return nil, err
	}
	d, err := s.Reads.Doc(ctx, id, artifactdataapi.Reading(a, fresh), p.String("collection"), p.String("doc"))
	return d, dataFailure(err)
}

// query answers a page of a query over a collection's field index: `q=` the
// query as JSON ([query.Query]).
func (s artifactData) query(ctx context.Context, p Params) (any, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	var q query.Query
	if raw := p.String("q"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &q); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return nil, fmt.Errorf("%w: %s. It takes a query as JSON", ErrBadParams,
				plainjson.Unreadable("the q parameter", err))
		}
	}
	id, a, err := s.admit(ctx, "artifact_data_query", authz.ActionArtifactDataQuery, p)
	if err != nil {
		return nil, err
	}
	page, err := s.Reads.Query(ctx, id, artifactdataapi.Reading(a, fresh), p.String("collection"), q)
	return page, dataFailure(err)
}

// history answers a page of a store's history, narrowed by `collection=`,
// `doc=`, `field=` (a cell's history) or `actor=`.
func (s artifactData) history(ctx context.Context, p Params) (any, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	id, a, err := s.admit(ctx, "artifact_data_history", authz.ActionArtifactDataRead, p)
	if err != nil {
		return nil, err
	}
	page, err := s.Reads.History(ctx, id, artifactdataapi.Reading(a, fresh), artifactdata.HistoryFilter{
		Collection: p.String("collection"), Doc: p.String("doc"), Field: p.String("field"),
		Actor: p.String("actor"),
	}, p.String("cursor"), p.Int("limit", 0))
	return page, dataFailure(err)
}

// schemaAnswer is a store's collections, its catalogue row (the footprint and
// the state digest the data tab shows) and the reader's role, which decides
// what the dashboard offers to edit.
type schemaAnswer struct {
	Collections []artifacts.Collection `json:"collections"`
	Store       artifactdata.StoreInfo `json:"store"`
	Role        artifacts.Role         `json:"role"`
}

// schema answers a store's collections and its catalogue row.
func (s artifactData) schema(ctx context.Context, p Params) (any, error) {
	id, a, err := s.admit(ctx, "artifact_data_schema", authz.ActionArtifactDataRead, p)
	if err != nil {
		return nil, err
	}
	collections, err := s.Reads.Schema(ctx, id)
	if err != nil {
		return nil, dataFailure(err)
	}
	// A COLLECTION THE READER MAY NEVER READ A ROW OF is left out of the
	// schema they are shown: its name and columns are the editors' too.
	visible := make([]artifacts.Collection, 0, len(collections))
	for _, c := range collections {
		if c.Read != artifacts.ReadEditors || a.Role().Includes(artifacts.RoleEditor) {
			visible = append(visible, c)
		}
	}
	info, err := s.Reads.Store(ctx, id)
	if err != nil {
		return nil, dataFailure(err)
	}
	return schemaAnswer{Collections: visible, Store: info, Role: a.Role()}, nil
}

// state answers the caller's own per-viewer state keys.
func (s artifactData) state(ctx context.Context, p Params) (any, error) {
	fresh, err := Freshness(p)
	if err != nil {
		return nil, err
	}
	id, a, err := s.admit(ctx, "artifact_data_state", authz.ActionArtifactDataRead, p)
	if err != nil {
		return nil, err
	}
	entries, err := s.Reads.State(ctx, id, artifactdataapi.Reading(a, fresh))
	return map[string]any{"state": entries}, dataFailure(err)
}

// usage answers the company's datastore counters against the plan quota.
func (s artifactData) usage(ctx context.Context, _ Params) (any, error) {
	principal, err := caller(ctx, "artifact_data_usage")
	if err != nil {
		return nil, err
	}
	d := authz.Decide(ctx, principal, authz.ActionArtifactUsage, authz.Object{Kind: authz.KindArtifact},
		s.Gate.Chart, s.now())
	switch {
	case d.Unknown():
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, d.Err)
	case !d.Allowed:
		return nil, NewRefusal("artifact_data_usage", d)
	}
	usage, err := s.Reads.Usage(ctx)
	if err != nil {
		return nil, dataFailure(err)
	}
	var limits artifacts.Limits
	if s.Gate.Limits != nil {
		limits = s.Gate.Limits()
	}
	return map[string]any{"usage": usage, "data_bytes": limits.DataBytes,
		"data_store_bytes": limits.DataStoreBytes}, nil
}
