package artifactsapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE COMPANY'S BRAND ON THE API ORIGIN (ADR-0048): the adopted brand, which
// everybody reads (`get_company_brand`), its files, and the administrator's
// adoption and clear (`artifacts.brand.adopt`, `.clear`: an administrator, a
// person, at a recent proof of identity, decided by each route's policy).
//
// THE ADOPTED BRAND IS THE COMPANY'S PUBLIC FACE, so its answer is the same for
// every reader but one field: whether a kit version newer than the adopted one
// exists, and who published it, is answered only to a reader who may open the
// kit (by a relation, or as an administrator), since a draft is not the public
// face. Telling an administrator reads the kit's head and no content, so it
// leaves no elevated-read record.
//
// ITS FILES ARE SERVED HERE AS EVERY ARTIFACT FILE IS (ADR-0035's second rule):
// an attachment, a drawing as `text/plain`, under the API's own policy, so a
// logo opened from the dashboard can never run on the credential-bearing
// origin. Artifact pages read the same files from the public brand route on
// the artifact origin, by their hash.

// BrandReads is the adopted brand as the brand routes and the `brand` question
// read it: the estate router's.
type BrandReads interface {
	Brand(ctx context.Context, fresh statelog.Freshness) (artifactsdomain.Adoption, bool, error)
	BrandFile(ctx context.Context, hash objstore.Hash) (artifactsdomain.BrandFile, error)
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
}

// BrandAnswer is the adopted brand (`GET /brand`, the `brand` question), or
// `{"brand": null}` when none is adopted.
type BrandAnswer struct {
	Brand *AdoptedBrand `json:"brand"`
}

// AdoptedBrand is the company's adopted brand as a reader is shown it.
type AdoptedBrand struct {
	// Kit and KitVersion are the brand kit and the version adopted.
	Kit        artifacts.ID `json:"kit"`
	KitVersion int          `json:"kit_version"`

	// Document is the adopted brand.json as the brand format reads it.
	Document brandformat.Brand `json:"document"`

	// CSSSha is the generated stylesheet's hash: what a framed version
	// published now pins.
	CSSSha string `json:"css_sha"`

	// Files are every file the brand serves, the stylesheet first, each
	// read at `GET /brand/files/{hash}`.
	Files []BrandFileRef `json:"files"`

	AdoptedBy iam.Actor `json:"adopted_by"`
	AdoptedAt time.Time `json:"adopted_at"`

	// NewerVersion and NewerBy are the kit's newest version and its
	// publisher, where it is newer than the adopted one: the "Review and
	// adopt" banner. Answered only to a reader who may open the kit.
	NewerVersion int        `json:"newer_version,omitempty"`
	NewerBy      *iam.Actor `json:"newer_by,omitempty"`
}

// BrandFileRef is one file the adopted brand serves.
type BrandFileRef struct {
	// Slot is the file's place in the brand (brandformat.Slot).
	Slot        string     `json:"slot"`
	ContentType string     `json:"content_type"`
	Object      BrandBytes `json:"object"`
}

// BrandBytes is a brand file's bytes as a reader names them: their SHA-256
// (the address both brand routes serve them at) and their size. The object's
// key is the estate's, never a reader's.
type BrandBytes struct {
	Hash objstore.Hash `json:"hash"`
	Size int64         `json:"size"`
}

// Brand answers the adopted brand for p, read at fresh.
func (g Gate) Brand(ctx context.Context, reads BrandReads, p iam.Principal,
	fresh statelog.Freshness) (BrandAnswer, error) {

	a, adopted, err := reads.Brand(ctx, fresh)
	if err != nil || !adopted {
		return BrandAnswer{}, err
	}
	snapshot, err := brandformat.DecodeAdopted(a.BrandJSON)
	if err != nil {
		return BrandAnswer{}, fmt.Errorf("artifactsapi: the adopted brand does not read under "+
			"the brand format this build holds: %w", err)
	}
	out := &AdoptedBrand{Kit: a.Kit, KitVersion: a.KitVersion, Document: snapshot.Brand(), CSSSha: a.CSSSha,
		AdoptedBy: publicActor(a.AdoptedBy), AdoptedAt: a.AdoptedAt}
	for _, f := range a.Files {
		out.Files = append(out.Files, BrandFileRef{Slot: f.Slot, ContentType: f.ContentType,
			Object: BrandBytes{Hash: f.Object.Hash, Size: f.Object.Size}})
	}
	newer, by, err := g.newerKit(ctx, reads, p, a, fresh)
	if err != nil {
		return BrandAnswer{}, err
	}
	if newer > 0 {
		out.NewerVersion, out.NewerBy = newer, &by
	}
	return BrandAnswer{Brand: out}, nil
}

// newerKit is the adopted kit's newest version and its publisher where it is
// newer than the adopted one and p may open the kit; zero otherwise. A kit
// trashed or purged since has no newer version to offer.
func (g Gate) newerKit(ctx context.Context, reads BrandReads, p iam.Principal,
	a artifactsdomain.Adoption, fresh statelog.Freshness) (int, iam.Actor, error) {

	head, _, err := g.Heads.Artifact(ctx, a.Kit, fresh)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return 0, iam.Actor{}, nil
	case err != nil:
		return 0, iam.Actor{}, err
	case head.Status != artifactsdomain.StatusLive || head.HeadNumber <= a.KitVersion:
		return 0, iam.Actor{}, nil
	}
	var refused *Refused
	switch _, decided := g.Decide(ctx, p, authz.ActionArtifactRead, head); {
	case errors.Is(decided, ErrNoSuchArtifact), errors.As(decided, &refused):
		return 0, iam.Actor{}, nil
	case decided != nil:
		return 0, iam.Actor{}, decided
	}
	v, err := reads.Version(ctx, a.Kit, head.HeadNumber)
	if err != nil {
		return 0, iam.Actor{}, err
	}
	return head.HeadNumber, publicActor(v.Author), nil
}

// publicActor is an author as the public brand names them: who, and what kind,
// never the credential they acted through.
func publicActor(a iam.Actor) iam.Actor { return iam.Actor{Name: a.Name, Kind: a.Kind} }

// brandRead is the level the brand routes read at: the dashboard's.
var brandRead = statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceDashboard)}

// brand answers the adopted brand (`GET /brand`), decided by
// `get_company_brand` where it is mounted.
func (s *Service) brand(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionBrandRead)
	if !ok {
		return
	}
	answer, err := s.gate.Brand(r.Context(), s.reads, p, brandRead)
	if err != nil {
		readFailed(w, r, err, "the adopted brand")
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// brandFile answers one file the adopted brand serves (`GET
// /brand/files/{hash}`), by its SHA-256, decided by `get_company_brand`: a
// hash no adoption serves is not found, so the route reads no object the
// brand does not name.
func (s *Service) brandFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := caller(w, r, authz.ActionBrandRead); !ok {
		return
	}
	hash := objstore.Hash(r.PathValue("hash"))
	if !hash.Valid() {
		notFound(w)
		return
	}
	f, err := s.reads.BrandFile(r.Context(), hash)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound):
		notFound(w)
		return
	case err != nil:
		readFailed(w, r, err, "the brand file")
		return
	}
	essence, _, _ := strings.Cut(f.ContentType, ";")
	ext, _ := brandformat.Ext(strings.TrimSpace(essence))
	entry := artifacts.Entry{Path: "brand-" + f.Slot + "." + ext, ContentType: f.ContentType, Object: f.Object}
	body, err := s.objects.Open(r.Context(), f.Object)
	if err == nil {
		defer body.Close()
		head := make([]byte, min(int64(downloadHead), f.Object.Size))
		if _, err = io.ReadFull(body, head); err == nil {
			s.sendFile(w, r, artifacts.ID{}, entry, io.MultiReader(bytes.NewReader(head), body))
			return
		}
	}
	contentFailed(w, r, artifacts.ID{}, entry.Path, err)
}

// adoptBody is one version of a kit to adopt.
type adoptBody struct {
	Kit     artifacts.ID `json:"kit"`
	Version int          `json:"version"`
}

// adoptBrand adopts one version of a brand kit as the company's brand (`POST
// /brand/adoptions`), decided by `artifacts.brand.adopt` where it is mounted.
// Reading the kit's content is decided as every content read is: an
// administrator with no relation to the kit is admitted on the admin path, and
// the override is recorded before anything is read.
func (s *Service) adoptBrand(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionBrandAdopt)
	if !ok {
		return
	}
	var body adoptBody
	if !decode(w, r, &body, false) {
		return
	}
	if body.Kit.IsZero() || body.Version <= 0 {
		invalid(w, "an adoption names the kit and its version: {\"kit\": <id>, \"version\": <n>}")
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	if _, err := s.gate.Content(r.Context(), p, authz.ActionArtifactRead, body.Kit, body.Version); err != nil {
		refuseAdmission(w, r, authz.ActionArtifactRead, err)
		return
	}
	pipe, ok := s.pipeline(w, r)
	if !ok {
		return
	}
	out, err := pipe.AdoptBrand(r.Context(), s.brandCall(p, key), body.Kit, body.Version)
	if err != nil {
		brandFailed(w, r, key, "adopt the brand", err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"kit": body.Kit.String(), "version": body.Version})
}

// clearBrand withdraws the company's adopted brand (`DELETE
// /brand/adoptions/current`), decided by `artifacts.brand.clear` where it is
// mounted. A page pinned to the adopted stylesheet keeps it while its version
// is retained.
func (s *Service) clearBrand(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r, authz.ActionBrandClear)
	if !ok {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	out, err := s.writes.ClearBrand(r.Context(), s.brandCall(p, key))
	if err != nil {
		brandFailed(w, r, key, "clear the brand", err)
		return
	}
	written(w, key, out.Result, nil)
}

// brandCall is an administrator's brand write: the actor and the operation,
// with the limits in force for the reservation the adoption's copies take. It
// names no role, since the brand is no artifact anybody holds a role on.
func (s *Service) brandCall(p iam.Principal, key string) artifactsdomain.Call {
	return artifactsdomain.Call{
		Authority: artifacts.Authority{Actor: iam.ActorFor(p), Limits: s.gate.limits(), Origin: originOf(p)},
		OpKey:     key,
	}
}

// brandFailed renders a brand write's refusal: an unreadable brand with the
// pair that refuses it, everything else as any write's.
func brandFailed(w http.ResponseWriter, r *http.Request, key, verb string, err error) {
	var (
		contrast   *artifactsdomain.BrandContrast
		incomplete *artifactsdomain.BrandIncomplete
	)
	if errors.As(err, &incomplete) {
		// A BRAND THAT LEAVES A FIELD OUT is answered as a kit outside its
		// format is, a finding per field, and the fields as a list beside
		// them for a surface that sends its reader to the step that sets one.
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodePolicyRefused,
			httpjson.Detail{"detail": say(err), "op_id": key,
				"findings": artifactupload.BrandFindings(brandformat.AdoptionFindings(incomplete.Missing)),
				"missing":  incomplete.Missing})
		return
	}
	if errors.As(err, &contrast) {
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeBrandContrast,
			httpjson.Detail{"detail": say(err), "op_id": key, "contrast": contrast.Pair})
		return
	}
	writeFailed(w, r, key, verb, err)
}
