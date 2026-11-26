package googleplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

// maxSteps bounds the changes one edit commits: a release with its notes in a
// handful of languages, or a listing in a dozen.
const maxSteps = 20

// Run makes a read call; every other call is prepared and applied
// ([Product.Prepare], [Product.Apply]).
func (p *Product) Run(ctx context.Context, call growth.Call) (any, error) {
	switch call.Tool {
	case toolListApps:
		return p.listApps(ctx, call)
	case toolListReviews:
		return p.listReviews(ctx, call)
	case toolReadListing:
		return p.readListing(ctx, call)
	case toolReadTracks:
		return p.readTracks(ctx, call)
	case toolQueryVitals:
		return p.queryVitals(ctx, call)
	case toolReadReport:
		return p.readReport(ctx, call)
	case toolRead:
		return p.readGeneric(ctx, call)
	}
	return nil, refusal(growth.RefusedInvalid, call.Tool+" changes Google Play, so it is prepared and applied, "+
		"never run as a read. No vendor was reached.")
}

// readGeneric makes google_play_read's call: a staged read in an edit the
// engine opens and discards, a dry run of staged changes, a reports bucket
// read as the company's reader, or a plain read.
func (p *Product) readGeneric(ctx context.Context, call growth.Call) (any, error) {
	req, err := p.genericRequest(call, call.Args)
	if err != nil {
		return nil, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return nil, err
	}
	switch {
	case req.Op.ID == "androidpublisher.edits.validate":
		return p.dryRun(ctx, call, req)
	case staged(req.Op):
		return p.readStaged(ctx, call, req)
	case strings.HasPrefix(req.Op.ID, "storage."):
		return p.readBucket(ctx, call, req)
	case strings.HasSuffix(req.Op.ID, ".download"):
		return p.download(ctx, call, req)
	}
	out, err := p.client.SendAndRead(ctx, call.Handle, req, p.cut(call.Handle, req))
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Download is a binary's account: Google Play's signed APK is never handed to
// an agent, but its size and digest say which build Google made.
type Download struct {
	Operation string `json:"operation"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Note      string `json:"note"`
}

// download reads a binary Google Play makes, counting and hashing it.
func (p *Product) download(ctx context.Context, call growth.Call, req googlegrowth.Request) (any, error) {
	req.Query.Set("alt", "media")
	h := &hashing{sum: sha256.New()}
	err := p.client.SendTo(ctx, call.Handle, req, h)
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return nil, err
	}
	return Download{Operation: req.Op.ID, Bytes: h.n, SHA256: hex.EncodeToString(h.sum.Sum(nil)),
		Note: "The file stays with Google Play: an agent never receives a binary."}, nil
}

// hashing counts and hashes what is written to it.
type hashing struct {
	sum hash.Hash
	n   int64
}

func (h *hashing) Write(b []byte) (int, error) {
	h.n += int64(len(b))
	return h.sum.Write(b)
}

// genericRequest builds a generic tool's request from args (a call's, or one
// of its steps'), refusing an operation the tool does not list.
func (p *Product) genericRequest(call growth.Call, args map[string]any) (googlegrowth.Request, error) {
	tool, ok := p.Catalog().Tool(call.Tool)
	if !ok {
		return googlegrowth.Request{}, refusal(growth.RefusedNotFound, call.Tool+" is not a Google Play tool.")
	}
	id, _ := args["operation"].(string)
	if !slices.Contains(tool.Operations, id) {
		return googlegrowth.Request{}, p.Catalog().Outside(call.Tool, id, p.Level(call.Handle))
	}
	op, _ := p.Catalog().Operation(id)
	return newRequest(op, stringArgs(args["path"]), stringArgs(args["query"]), objectArg(args["body"]))
}

// stepRequest builds one staged change of a commit or a dry run.
func (p *Product) stepRequest(args map[string]any) (googlegrowth.Request, error) {
	id, _ := args["operation"].(string)
	op, ok := p.Catalog().Operation(id)
	if !ok || !staged(op) || op.Class == growth.ClassRead || !op.Class.Callable() {
		return googlegrowth.Request{}, refusal(growth.RefusedInvalid, fmt.Sprintf("a step names %q: name a "+
			"change staged inside an edit (androidpublisher.edits.<resource>.<method>). No vendor was reached.", id))
	}
	return newRequest(op, stringArgs(args["path"]), stringArgs(args["query"]), objectArg(args["body"]))
}

// cut is how an answer to req is cut to handle's apps: only where req lists
// apps the identity reaches beyond the seat's.
func (p *Product) cut(handle string, req googlegrowth.Request) func(any) any {
	if req.Op.ID != "playdeveloperreporting.apps.search" {
		return nil
	}
	return func(v any) any {
		b := p.seat(handle)
		obj, ok := v.(map[string]any)
		if b == nil || !ok {
			return map[string]any{}
		}
		if list, ok := obj["apps"].([]any); ok {
			obj["apps"] = slices.DeleteFunc(list, func(e any) bool {
				m, _ := e.(map[string]any)
				name, _ := m["packageName"].(string)
				return !slices.Contains(b.Apps, name) || !p.company().Reaches(name)
			})
		}
		return obj
	}
}

// stringArgs is an argument object of strings.
func stringArgs(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, val := range m {
		switch t := val.(type) {
		case string:
			out[k] = t
		case float64, bool:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
}

// objectArg is an argument object, nil where none.
func objectArg(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// decode reads Google's JSON into out, naming what it was.
func decode(raw []byte, what string, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("googleplay: decode %s: %w", what, err)
	}
	return nil
}
