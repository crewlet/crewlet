package artifactdataapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("artifactdataapi")

// MaxBodyBytes bounds one request body: a batch's ops at their bound
// ([artifacts.MaxBatchBytes]) and 64 KiB for the rest of the envelope. A body
// over it but under what the domain accepts cannot exist; one under it but
// over the domain's own cap is refused by the domain, naming the field.
const MaxBodyBytes = artifacts.MaxBatchBytes + 64<<10

// Data is the datastore as this surface writes and exports it: the estate
// router's, so a node whose own copy is out of service writes and reads
// through a peer that serves.
type Data interface {
	Write(ctx context.Context, c artifactdata.Call, id artifacts.ID, in artifacts.DataWrite) (
		artifactdata.WriteResult, error)
	Undo(ctx context.Context, c artifactdata.Call, id artifacts.ID, seq int64) (artifactdata.WriteResult, error)
	Configure(ctx context.Context, c artifactdata.Call, id artifacts.ID, collections []artifacts.Collection) (
		artifactdata.WriteResult, error)
	Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error)
	Docs(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string, since int64,
		cursor string, limit int) (artifactdata.DocPage, error)
}

// Versions is the artifacts estate's read of one version: what a page script's
// write is checked against, the capabilities its version declares.
type Versions interface {
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
}

// Options configure the surface.
type Options struct {
	// Gate is the one admission every route takes. REQUIRED, with its
	// Heads, Chart, Limits and Marks (an export only the admin path admits
	// is recorded before a row is sent).
	Gate artifactsapi.Gate

	// Data is the datastore. REQUIRED.
	Data Data

	// Versions answers a version's declared capabilities. REQUIRED.
	Versions Versions

	// Names answers a seat handle's display name, which a person cell is
	// exported as. REQUIRED: an export is read by people, and a handle is
	// the chart's address for a seat, not its name.
	Names artifactdata.Names

	// Now is the clock an operation key is minted at; nil is the wall clock.
	Now func() time.Time
}

// Service is the surface.
type Service struct {
	gate     artifactsapi.Gate
	data     Data
	versions Versions
	names    artifactdata.Names
	now      func() time.Time
}

// New builds the surface.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Gate.Heads == nil:
		return nil, errors.New("artifactdataapi: no artifact heads: every route decides on " +
			"the artifact's shares")
	case opts.Gate.Chart == nil:
		return nil, errors.New("artifactdataapi: no chart: a share to a unit and the lead " +
			"arm are resolved through one; pass authz.NoChart to decide on grants alone")
	case opts.Gate.Limits == nil:
		return nil, errors.New("artifactdataapi: no plan quotas: every write is stamped " +
			"with the limits in force")
	case opts.Gate.Marks == nil:
		return nil, errors.New("artifactdataapi: no reader marks: an export only the admin " +
			"path admits is recorded before a row is sent")
	case opts.Data == nil:
		return nil, errors.New("artifactdataapi: no datastore")
	case opts.Versions == nil:
		return nil, errors.New("artifactdataapi: no versions: a page script's write is " +
			"checked against its version's declared capabilities")
	case opts.Names == nil:
		return nil, errors.New("artifactdataapi: no seat names: an export writes a person " +
			"cell as the seat's name")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{gate: opts.Gate, data: opts.Data, versions: opts.Versions, names: opts.Names, now: now}, nil
}

// Routes mounts the surface's write routes and the export.
//
// EVERY ROUTE ADMITS ON ITS WEAKEST HONEST PRECONDITION, which for an artifact
// is that the caller resolved and may act ([authz.ActionArtifactList]): the
// verb itself needs the artifact's shares, which only its head can say, so
// each handler reads the head and decides the verb on it through the one
// [artifactsapi.Gate], the artifacts' own. A caller with no relation is
// answered exactly as an unknown artifact.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(s.gate.Chart)).Refusing(authz.EnvelopeRefusal)
	pre := authz.Policy{Action: authz.ActionArtifactList, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	var failures []error
	for _, route := range []struct {
		pattern string
		handler http.HandlerFunc
		absent  *authz.Absent
	}{
		{"POST /artifacts/{id}/data/writes", s.writes, nil},
		{"POST /artifacts/{id}/data/undo", s.undo, nil},
		{"POST /artifacts/{id}/data/state", s.state, nil},
		// A SCHEMA BELONGS TO AN ARTIFACT, and replacing it makes none.
		{"PUT /artifacts/{id}/data/schema", s.schema, authz.AddedElsewhere(
			"An artifact is created with POST /artifacts, and its data schema is written here once it exists.")},
		{"GET /artifacts/{id}/data/export.csv", s.export, nil},
	} {
		if err := router.Handle(route.pattern, pre.WhenAbsent(route.absent), route.handler); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// admit decides action on the path's artifact for a write, rendering a
// refusal itself. It reports whether the handler goes on.
func (s *Service) admit(w http.ResponseWriter, r *http.Request, action authz.Action) (
	artifacts.ID, artifactsapi.Admission, bool) {

	return s.admitWith(w, r, action, s.gate.Admit)
}

// admitRead decides action on the path's artifact as a read of its data
// ([artifactsapi.Gate.Content]): an artifact in the trash is refused, and an
// administrator only the admin path admits is recorded before a row is sent.
func (s *Service) admitRead(w http.ResponseWriter, r *http.Request, action authz.Action) (
	artifacts.ID, artifactsapi.Admission, bool) {

	return s.admitWith(w, r, action, func(ctx context.Context, p iam.Principal, action authz.Action,
		id artifacts.ID) (artifactsapi.Admission, error) {
		return s.gate.Content(ctx, p, action, id, 0)
	})
}

// admitWith resolves the caller and the path's artifact and admits them
// through one of the gate's admissions, rendering a refusal itself.
func (s *Service) admitWith(w http.ResponseWriter, r *http.Request, action authz.Action,
	admit func(context.Context, iam.Principal, authz.Action, artifacts.ID) (artifactsapi.Admission, error)) (
	artifacts.ID, artifactsapi.Admission, bool) {

	id, err := artifacts.ParseID(r.PathValue("id"))
	if err != nil {
		// A PATH NAMING NO ARTIFACT ID is an unknown artifact.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return artifacts.ID{}, artifactsapi.Admission{}, false
	}
	principal, how := iam.From(r.Context())
	if how != iam.Resolved {
		operator.RefuseDecision(w, r, action, authz.Decision{Err: iam.Reason(r.Context())})
		return id, artifactsapi.Admission{}, false
	}
	a, err := admit(r.Context(), principal, action, id)
	var refused *artifactsapi.Refused
	switch {
	case err == nil:
		return id, a, true
	case errors.Is(err, artifactsapi.ErrNoSuchArtifact):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case errors.As(err, &refused):
		operator.RefuseDecision(w, r, action, refused.Decision)
	case errors.Is(err, artifactsdomain.ErrTrashed):
		dataGone(w, "the artifact is in the trash; its data is not served until it is restored")
	case errors.Is(err, artifactsapi.ErrRecordUnwritten):
		log.WarnContext(r.Context(), "artifacts_elevated_read_unrecorded", "artifact", id.String(),
			"error", err.Error())
		unavailable(w, err, "this read is admitted only as an administrator's, and the record "+
			"of it could not be written, so it was not answered")
	default:
		unavailable(w, err, "this node could not decide who may act on this artifact's data")
	}
	return id, artifactsapi.Admission{}, false
}

// writable refuses a write to an artifact in the trash: its data is not
// served, so nothing is written to it until it is restored.
func writable(w http.ResponseWriter, a artifactsapi.Admission) bool {
	if a.Head.Status == artifactsdomain.StatusTrashed {
		dataGone(w, "the artifact is in the trash; restore it to write its data")
		return false
	}
	return true
}

// dataGone is `410 data_gone`: an artifact whose data is not served.
func dataGone(w http.ResponseWriter, detail string) {
	httpjson.FailWith(w, http.StatusGone, httpjson.CodeDataGone, map[string]string{"detail": detail})
}

// Via is the surface a write declares it came through. A browser session
// declares the dashboard's own screens or a page script relayed by the host
// bridge; a bearer credential (the CLI, a script) is the API whatever it says.
type Via string

// The surfaces a write is stamped as coming through.
const (
	ViaDashboard Via = "dashboard"
	ViaBridge    Via = "bridge"
)

// origin is the write origin a request is stamped with, and for a page
// script's write, the capability its version must declare.
func (s *Service) origin(w http.ResponseWriter, r *http.Request, a artifactsapi.Admission, id artifacts.ID,
	via Via, version int, capability artifacts.Capability) (artifacts.WriteOrigin, bool) {

	if !strings.HasPrefix(a.Principal.Via, iam.SessionPrefix) {
		return artifacts.OriginAPI, true
	}
	switch via {
	case "", ViaDashboard:
		return artifacts.OriginDashboard, true
	case ViaBridge:
		// A PAGE SCRIPT'S WRITE names the version it runs as, and that
		// version must declare the capability the write uses (03 section
		// 4.3): a page published without `db` reads and writes no data.
		if version <= 0 {
			invalid(w, "a page script's write names the version it runs as")
			return "", false
		}
		v, err := s.versions.Version(r.Context(), id, version)
		switch {
		case errors.Is(err, artifactsdomain.ErrNotFound):
			invalid(w, "the version the page names is not one this artifact keeps")
			return "", false
		case err != nil:
			unavailable(w, err, "this node could not read the page's version")
			return "", false
		case !slices.Contains(v.Capabilities, capability):
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden, map[string]string{
				"detail": "version " + strconv.Itoa(version) + " declares no `" + string(capability) +
					"` capability, so its page writes no data"})
			return "", false
		}
		return artifacts.OriginBridge, true
	}
	invalid(w, "via is dashboard or bridge")
	return "", false
}

// call is a write's caller context: the admission's stamp and the request's
// key.
func (s *Service) call(a artifactsapi.Admission, origin artifacts.WriteOrigin, key string) artifactdata.Call {
	return artifactdata.Call{Authority: authority(a, origin, s.gate.Limits()), OpKey: key}
}

// decode reads a JSON body into out, refusing a field the route does not take:
// a typo would otherwise read as a request for less.
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	body, err := httpjson.ReadBody(w, r, MaxBodyBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return false
	}
	return true
}

// writeBody is a batch of ops.
type writeBody struct {
	Ops      []artifacts.DataOp `json:"ops"`
	ClientID string             `json:"client_id,omitempty"`
	Via      Via                `json:"via,omitempty"`
	Version  int                `json:"version,omitempty"`
}

// writes publishes one batch of ops (`POST /artifacts/{id}/data/writes`).
func (s *Service) writes(w http.ResponseWriter, r *http.Request) {
	id, a, ok := s.admit(w, r, authz.ActionArtifactDataWrite)
	if !ok || !writable(w, a) {
		return
	}
	var body writeBody
	if !decode(w, r, &body) {
		return
	}
	capability := artifacts.CapabilityDB
	if len(body.Ops) > 0 && !slices.ContainsFunc(body.Ops, func(op artifacts.DataOp) bool {
		return op.Collection != artifactdata.StateCollection
	}) {
		capability = artifacts.CapabilityState
	}
	origin, ok := s.origin(w, r, a, id, body.Via, body.Version, capability)
	if !ok {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	res, err := s.data.Write(r.Context(), s.call(a, origin, key), id,
		artifacts.DataWrite{Ops: body.Ops, ClientID: body.ClientID})
	answer(w, r, key, res, err)
}

// undoBody names the write to undo.
type undoBody struct {
	Seq      int64  `json:"seq"`
	ClientID string `json:"client_id,omitempty"`
	Via      Via    `json:"via,omitempty"`
	Version  int    `json:"version,omitempty"`
}

// undo publishes the inverse of one write (`POST /artifacts/{id}/data/undo`).
func (s *Service) undo(w http.ResponseWriter, r *http.Request) {
	id, a, ok := s.admit(w, r, authz.ActionArtifactDataWrite)
	if !ok || !writable(w, a) {
		return
	}
	var body undoBody
	if !decode(w, r, &body) {
		return
	}
	origin, ok := s.origin(w, r, a, id, body.Via, body.Version, artifacts.CapabilityDB)
	if !ok {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	res, err := s.data.Undo(r.Context(), s.call(a, origin, key), id, body.Seq)
	answer(w, r, key, res, err)
}

// stateBody is one per-viewer state key's new value, or its removal.
type stateBody struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value,omitempty"`
	Delete    bool            `json:"delete,omitempty"`
	IfVersion *int64          `json:"if_version,omitempty"`
	ClientID  string          `json:"client_id,omitempty"`
	Via       Via             `json:"via,omitempty"`
	Version   int             `json:"version,omitempty"`
}

// state sets or removes one of the caller's own per-viewer state keys (D15):
// the backing of the runtime's state API, kept in the engine's private state
// collection under the caller's record (`POST /artifacts/{id}/data/state`).
func (s *Service) state(w http.ResponseWriter, r *http.Request) {
	id, a, ok := s.admit(w, r, authz.ActionArtifactDataWrite)
	if !ok || !writable(w, a) {
		return
	}
	var body stateBody
	if !decode(w, r, &body) {
		return
	}
	doc, err := artifactdata.StateDocID(iam.RecordOwner(a.Principal), body.Key)
	if err != nil {
		invalid(w, strings.TrimPrefix(err.Error(), artifactdata.ErrInvalid.Error()+": "))
		return
	}
	op := artifacts.DataOp{Op: artifacts.DataDelete, Collection: artifactdata.StateCollection,
		Doc: doc, IfVersion: body.IfVersion}
	switch {
	case body.Delete && len(body.Value) > 0:
		invalid(w, "a state write sets a value or deletes the key, never both")
		return
	case !body.Delete:
		if len(body.Value) == 0 {
			invalid(w, "a state write names its value")
			return
		}
		op = artifacts.DataOp{Op: artifacts.DataSet, Collection: artifactdata.StateCollection,
			Doc: doc, IfVersion: body.IfVersion,
			Fields: map[string]json.RawMessage{artifactdata.StateField: body.Value}}
	}
	origin, ok := s.origin(w, r, a, id, body.Via, body.Version, artifacts.CapabilityState)
	if !ok {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	res, err := s.data.Write(r.Context(), s.call(a, origin, key), id,
		artifacts.DataWrite{Ops: []artifacts.DataOp{op}, ClientID: body.ClientID})
	answer(w, r, key, res, err)
}

// schemaBody is every collection the store keeps after a schema change.
type schemaBody struct {
	Collections []artifacts.Collection `json:"collections"`
}

// schema replaces the store's collections (`PUT /artifacts/{id}/data/schema`).
func (s *Service) schema(w http.ResponseWriter, r *http.Request) {
	id, a, ok := s.admit(w, r, authz.ActionArtifactSchema)
	if !ok || !writable(w, a) {
		return
	}
	var body schemaBody
	if !decode(w, r, &body) {
		return
	}
	origin, ok := s.origin(w, r, a, id, "", 0, artifacts.CapabilityDB)
	if !ok {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	res, err := s.data.Configure(r.Context(), s.call(a, origin, key), id, body.Collections)
	answer(w, r, key, res, err)
}

// export answers one collection as CSV (`GET
// /artifacts/{id}/data/export.csv?collection=`), under the collection's read
// rule, to an editor: an export is every row at once, which is the reason the
// row admits editors and above only. A read of the artifact's content: never
// of one in the trash, and recorded first for an administrator only the admin
// path admits.
func (s *Service) export(w http.ResponseWriter, r *http.Request) {
	id, a, ok := s.admitRead(w, r, authz.ActionArtifactExport)
	if !ok {
		return
	}
	collection := r.URL.Query().Get("collection")
	if collection == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "name the collection to export as collection="})
		return
	}
	out := &csvResponse{w: w, collection: collection}
	err := artifactdata.Export(r.Context(), s.data, id, Reading(a, statelog.Freshness{
		Level: statelog.DefaultReadLevel(statelog.SurfaceDashboard)}), collection, s.names, out)
	switch {
	case err == nil && !out.started:
		// AN EXPORT ALWAYS WRITES ITS HEADER ROW, so this is unreachable;
		// answered as an empty file rather than a body-less 200.
		out.start()
	case err != nil && !out.started:
		readFailed(w, err)
	case err != nil:
		// A STORE THAT STOPPED ANSWERING HALF-WAY: the rows already sent
		// cannot be recalled, so the connection is cut rather than ended
		// cleanly, and the client sees a truncated download instead of a
		// file that looks whole.
		panic(http.ErrAbortHandler)
	}
}

// csvResponse streams an export, writing the attachment's headers on the
// first byte: the export is written a page at a time, so a store of a
// gigabyte is never held in memory, and a failure before the first page is
// still answered as a refusal.
type csvResponse struct {
	w          http.ResponseWriter
	collection string
	started    bool
}

func (c *csvResponse) start() {
	c.started = true
	h := c.w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+c.collection+`.csv"`)
	h.Set("X-Content-Type-Options", "nosniff")
	c.w.WriteHeader(http.StatusOK)
}

func (c *csvResponse) Write(p []byte) (int, error) {
	if !c.started {
		c.start()
	}
	return c.w.Write(p)
}

// answer renders a write's outcome: the verdict the applier wrote, the pending
// position, or the refusal before anything was appended.
//
// ONE FIELD SAYS HOW THE WRITE ENDED, `verdict`, and `pending` is one of its
// values. The state log's own three-valued outcome is NOT answered beside it:
// a refused write is a record the log applied, so its `outcome` was `applied`
// next to `"error":"invalid"`, which read as a write that both landed and did
// not. The verdict is the answer a caller acts on, exactly as the agent's data
// tool and the page runtime say it.
func answer(w http.ResponseWriter, r *http.Request, key string, res artifactdata.WriteResult, err error) {
	if err != nil {
		writeFailed(w, r, key, err)
		return
	}
	body := httpjson.Detail{"op_id": key, "position": res.Result.Position}
	switch res.Result.Outcome {
	case statelog.OutcomePending:
		// THE RECORD LANDED and this node has not applied it yet: the
		// verdict is on the position, read by catching up past it.
		body["verdict"] = verdictPending
		httpjson.Write(w, http.StatusAccepted, body)
		return
	case statelog.OutcomeUnknown:
		operator.UnknownOutcome(w, key, res.Result.Unvouched, map[string]any{"op_id": key})
		return
	}
	body["verdict"], body["seq"] = string(res.Verdict), res.Seq
	if res.Reason != "" {
		body["detail"] = res.Reason
	}
	switch res.Verdict {
	case artifacts.DataApplied:
		httpjson.Write(w, http.StatusOK, body)
	case artifacts.DataConflict:
		body["conflict"] = res.Conflict
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeStaleVersion, body)
	case artifacts.DataInvalid:
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid, body)
	case artifacts.DataForbidden:
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeForbidden, body)
	case artifacts.DataQuota:
		httpjson.FailWithFields(w, http.StatusRequestEntityTooLarge, httpjson.CodeDataQuota, body)
	case artifacts.DataGone:
		httpjson.FailWithFields(w, http.StatusGone, httpjson.CodeDataGone, body)
	default:
		// A VERDICT THIS BUILD DOES NOT KNOW, written by a newer peer: the
		// record landed and changed what that build decided, which this
		// one cannot describe.
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict, body)
	}
}

// verdictPending is the verdict a write answers while the node that took it
// has not applied it: never one the applier writes, so it is not an
// [artifacts.DataVerdict].
const verdictPending = "pending"

// writeFailed renders a write refused before anything was appended, or one
// whose outcome nobody can establish.
func writeFailed(w http.ResponseWriter, r *http.Request, key string, err error) {
	var limited *artifactdata.RateLimited
	detail := map[string]string{"detail": strings.TrimPrefix(err.Error(), "artifactdata: ")}
	switch {
	case errors.As(err, &limited):
		httpjson.ThrottledWith(w, httpjson.CodeWriteBudget, limited.RetryAfter, httpjson.Detail{
			"detail": detail["detail"], "scope": limited.Scope,
			"retry_after_ms": limited.RetryAfter.Milliseconds()})
	case errors.Is(err, artifactdata.ErrGone):
		httpjson.FailWith(w, http.StatusGone, httpjson.CodeDataGone, detail)
	case errors.Is(err, artifactdata.ErrForbidden):
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden, detail)
	case errors.Is(err, artifactdata.ErrNotUndoable):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNotUndoable, detail)
	case errors.Is(err, artifactdata.ErrMigrationTooLarge):
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeMigrationTooLarge, detail)
	case errors.Is(err, artifactdata.ErrConflict):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, detail)
	case errors.Is(err, artifactdata.ErrNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, detail)
	case errors.Is(err, artifactdata.ErrInvalid):
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid, detail)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		operator.Interrupted(w, r, key, "write_artifact_data", err)
	default:
		unavailable(w, err, "this node could not write the artifact's data")
	}
}

// readFailed renders a read that could not be answered.
func readFailed(w http.ResponseWriter, err error) {
	detail := map[string]string{"detail": strings.TrimPrefix(err.Error(), "artifactdata: ")}
	switch {
	case errors.Is(err, artifactdata.ErrGone):
		httpjson.FailWith(w, http.StatusGone, httpjson.CodeDataGone, detail)
	case errors.Is(err, artifactdata.ErrNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, detail)
	case errors.Is(err, artifactdata.ErrInvalid), errors.Is(err, artifactdata.ErrBadCursor):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams, detail)
	default:
		unavailable(w, err, "this node could not read the artifact's data")
	}
}

// invalid is a request the route refuses on its own rules.
func invalid(w http.ResponseWriter, detail string) {
	httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
		map[string]string{"detail": detail})
}

// unavailable is a 503 whose cause says whether, and when, to come back.
func unavailable(w http.ResponseWriter, cause error, detail string) {
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
		httpjson.RetrySeconds(statelog.RetryAfter(cause, authz.RetryUndecidedSeconds*time.Second)),
		httpjson.Detail{"detail": detail})
}
