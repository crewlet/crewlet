package artifactsdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The artifact's own writes: the create, a publish and a rollback, a patch,
// shares, a transfer, a link to a work item, the trash and the purge.

// Create makes an artifact and its first version in one record.
//
// THE ID IS THE OPERATION'S ([artifacts.IDFor]), so the retry an unknown
// outcome asks for names the artifact its first attempt created. With a source
// key the record arbitrates on the ADDRESS, create-only: two writers publishing
// one source for the first time contend there and one wins, the other told
// [ErrSourceTaken].
func (w *Writer) Create(ctx context.Context, c Call, in CreateInput) (Published, error) {
	if err := validCall(c); err != nil {
		return Published{}, err
	}
	if err := quotaCall(c); err != nil {
		return Published{}, err
	}
	at := w.now()
	if err := checkCreate(c, in, at); err != nil {
		return Published{}, err
	}
	opID, err := w.operation(c, string(OpCreate), in.Owner+"/"+in.SourceKey, in)
	if err != nil {
		return Published{}, err
	}
	id := artifacts.IDFor(opID)
	versionID := versionIDOf(opID)
	subject, scope := createScope(id, in)
	pins := pinsBrand(in.Kind, in.First)
	if pins {
		scope = withBrand(ArtifactSubject(id), scope)
	}
	shares := make([]artifacts.Share, len(in.Shares))
	for i, s := range in.Shares {
		shares[i] = artifacts.Share{Audience: s.Audience, Role: s.Role,
			GrantedBy: c.Authority.Actor, GrantedAt: at}
	}
	first := in.First
	payload := CreatePayload{
		V: baseRecordVersion, Artifact: id.String(), Kind: in.Kind, Title: in.Title,
		Description: in.Description, Icon: in.Icon, Owner: in.Owner,
		OwnerKind: in.OwnerKind, SourceKey: in.SourceKey, Folder: in.Folder,
		Version: VersionPayload{
			Number: 1, VersionID: versionID.String(), Label: first.Label,
			Contract: first.Contract, Capabilities: canonicalCapabilities(first.Capabilities),
			Manifest: manifest.Canonical(first.Manifest), SearchText: first.SearchText,
		},
		Shares: shares, Authority: c.Authority,
	}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternCreate,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if in.SourceKey != "" {
				holder, held, err := sourceHolder(ctx, tx, in.Owner, SourceToken(in.SourceKey)) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
				if err != nil {
					return statelog.Decision{}, err
				}
				if held && holder != id.String() {
					return statelog.Decision{}, fmt.Errorf("%w: %s holds it", ErrSourceTaken, holder)
				}
			}
			if _, marked, err := deletionMarker(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			} else if marked {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrPurged, id)
			}
			if in.Folder != "" {
				live, liveErr := folderLive(ctx, tx, in.Folder)
				switch {
				case liveErr != nil:
					return statelog.Decision{}, liveErr
				case !live:
					return statelog.Decision{}, fmt.Errorf("%w: %s", ErrFolderNotFound, in.Folder)
				}
			}
			plan, refusal, err := planObjects(ctx, tx, id.String(), payload.Version.Manifest.Files, nil) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if refusal != nil {
				return statelog.Decision{}, planRefusal(refusal)
			}
			if err := checkQuota(ctx, tx, c.Authority.Limits, 1, plan.added); err != nil {
				return statelog.Decision{}, err
			}
			if pins {
				brand, read := currentCSSSha(ctx, tx)
				if read != nil {
					return statelog.Decision{}, read
				}
				payload.Version.Brand = brand
			}
			return w.decision(stamp, c, subject, OpCreate, scope, opID, payload, at)
		}))
	out := Published{Artifact: id, Number: 1, VersionID: versionID, Result: res}
	if err != nil {
		return out, err
	}
	e, found, err := w.verdict(ctx, id.String(), opID, res)
	if err != nil || !found {
		return out, err
	}
	return out, w.refused(ctx, id.String(), e)
}

// createScope is a create's subject and scope: the artifact's own subject, or
// its source address's where it has one (both named), beside the folder it is
// created in, which the create reads and so orders behind any record about.
func createScope(id artifacts.ID, in CreateInput) (Subject, ScopeSet) {
	subject := ArtifactSubject(id)
	var terms []ScopeTerm
	if in.SourceKey != "" {
		subject = SourceSubject(in.Owner, in.SourceKey)
		terms = append(terms, ScopeTerm{Kind: TermSource, ID: subject.ID})
	}
	if in.SourceKey == "" && in.Folder == "" {
		return subject, ScopeSet{Subject: true}
	}
	terms = append(terms, artifactTerm(id.String()))
	if in.Folder != "" {
		terms = append(terms, FolderTerm(in.Folder))
	}
	return subject, ScopeSet{Terms: terms}
}

// checkCreate refuses a create the domain would never write.
func checkCreate(c Call, in CreateInput, at time.Time) error {
	if in.Folder != "" && !validFolderID(in.Folder) {
		return fmt.Errorf("%w: %q", ErrFolderNotFound, in.Folder)
	}
	if !in.Kind.Valid() {
		return fmt.Errorf("%w: kind %q is not one of %v", ErrInvalid, in.Kind, artifacts.Kinds)
	}
	if err := checkHead(&in.Title, &in.Description, &in.Icon, true); err != nil {
		return err
	}
	if err := checkName("the owner", in.Owner); err != nil {
		return err
	}
	if in.OwnerKind != iam.ActorAgent && in.OwnerKind != iam.ActorHuman &&
		in.OwnerKind != iam.ActorOperator {
		return fmt.Errorf("%w: owner kind %q: an artifact is owned by a seat (an "+
			"agent's or a person's) or an unbound machine", ErrInvalid, in.OwnerKind)
	}
	if err := checkText("the source key", in.SourceKey, artifacts.MaxSourceKeyBytes, false); err != nil {
		return err
	}
	if in.First.RestoredFrom != 0 || in.First.Force {
		return fmt.Errorf("%w: a create's first version restores nothing and forces "+
			"nothing", ErrInvalid)
	}
	if err := checkVersionInput(in.Kind, in.First); err != nil {
		return err
	}
	for _, e := range in.First.Manifest.Files {
		if e.Carried {
			return fmt.Errorf("%w: %q is carried, and a new artifact holds nothing to "+
				"carry from", ErrInvalid, e.Path)
		}
	}
	if err := checkUploads(in.First.Manifest, in.First.Uploaded, at); err != nil {
		return err
	}
	for _, s := range in.Shares {
		// THE CREATOR HOLDS EVERY ROLE ON WHAT THEY CREATE, so the share
		// ceiling at a create is the owner's.
		if err := checkShare(c, s, artifacts.RoleOwner); err != nil {
			return err
		}
	}
	return nil
}

// checkHead refuses displayed fields outside their bounds. Title is required
// where required is.
func checkHead(title, description, icon *string, required bool) error {
	if title != nil {
		if err := checkText("the title", *title, artifacts.MaxTitleBytes, required); err != nil {
			return err
		}
	}
	if description != nil {
		if err := checkText("the description", *description, artifacts.MaxDescriptionBytes, false); err != nil {
			return err
		}
	}
	if icon != nil {
		if err := checkText("the icon", *icon, artifacts.MaxIconBytes, false); err != nil {
			return err
		}
	}
	return nil
}

// checkVersionInput refuses a version's own fields outside their bounds and a
// manifest the kind may not hold.
func checkVersionInput(kind artifacts.Kind, in PublishInput) error {
	if err := checkText("the label", in.Label, artifacts.MaxLabelBytes, false); err != nil {
		return err
	}
	if err := checkText("the search text", in.SearchText, int(artifacts.MaxSearchTextBytes), false); err != nil {
		return err
	}
	if err := checkCapabilities(in.Capabilities); err != nil {
		return err
	}
	if err := manifest.Validate(kind, manifest.Canonical(in.Manifest)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// checkShare refuses a share the domain never writes: an audience that cannot
// be addressed, a role that is not shareable, one above the sharer's ceiling,
// and the company audience from an agent (D10). Whether the audience may be
// GRANTED is the write's own question, decided against what is stored
// ([Writer.Share]).
func checkShare(c Call, s artifacts.Share, ceiling artifacts.Role) error {
	if err := s.Audience.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !slices.Contains(artifacts.ShareableRoles, s.Role) {
		return fmt.Errorf("%w: role %q is not one a share grants (%v): ownership moves "+
			"only by transfer", ErrInvalid, s.Role, artifacts.ShareableRoles)
	}
	if !ceiling.Valid() {
		return fmt.Errorf("%w: the call's authority names no role on the artifact, so "+
			"no share could be held to it", ErrInvalid)
	}
	if !ceiling.Includes(s.Role) {
		return fmt.Errorf("%w: %s holds %s and cannot grant %s", ErrAboveOwnRole,
			c.Authority.Actor.Name, ceiling, s.Role)
	}
	if s.Audience.Kind == artifacts.AudienceCompany && c.Authority.Actor.Kind == iam.ActorAgent {
		return ErrAgentCompanyShare
	}
	return nil
}

// Publish adds a version decided against the head the caller names, or, with
// RestoredFrom, rolls back to an earlier version as a new one.
//
// THE NUMBER IS head + 1, decided in the artifact subject's snapshot, so the
// broker makes it unique. A head that moved under the caller is the 412 that
// carries the live head ([VersionMoved]), unless Force. The record names the
// oldest unlabelled versions past the hundred kept, retired in the same apply.
func (w *Writer) Publish(ctx context.Context, c Call, in PublishInput) (Published, error) {
	if err := validCall(c); err != nil {
		return Published{}, err
	}
	if err := quotaCall(c); err != nil {
		return Published{}, err
	}
	if in.Artifact.IsZero() {
		return Published{}, fmt.Errorf("%w: a publish names its artifact", ErrInvalid)
	}
	op := OpPublish
	if in.RestoredFrom != 0 {
		op = OpRestoreVersion
		if len(in.Manifest.Files) > 0 || in.Manifest.Entry != "" {
			return Published{}, fmt.Errorf("%w: a rollback names the version it restores "+
				"and no files of its own", ErrInvalid)
		}
	}
	if err := checkText("the label", in.Label, artifacts.MaxLabelBytes, false); err != nil {
		return Published{}, err
	}
	if err := checkText("the search text", in.SearchText, int(artifacts.MaxSearchTextBytes), false); err != nil {
		return Published{}, err
	}
	if err := checkCapabilities(in.Capabilities); err != nil {
		return Published{}, err
	}
	at := w.now()
	opID, err := w.operation(c, string(op), in.Artifact.String(), in)
	if err != nil {
		return Published{}, err
	}
	id := in.Artifact.String()
	versionID := versionIDOf(opID)
	subject, scope := ArtifactSubject(in.Artifact), ScopeSet{Subject: true}
	kind, err := w.kindOf(ctx, in.Artifact)
	if err != nil {
		return Published{}, err
	}
	pins := pinsBrand(kind, in)
	if namesBrand(kind) {
		scope = withBrand(subject, scope)
	}
	var number int
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Status == StatusTrashed {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
			}
			if !in.Force && in.Base != head.HeadNumber {
				return statelog.Decision{}, versionMovedTx(ctx, tx, id)
			}
			v, err := versionFor(ctx, tx, head, in, c.Authority.Limits, versionID.String(), at, pins)
			if err != nil {
				return statelog.Decision{}, err
			}
			number = v.Number
			return w.decision(stamp, c, subject, op, scope, opID,
				PublishPayload{V: baseRecordVersion, Version: v, Authority: c.Authority}, at)
		}))
	out := Published{Artifact: in.Artifact, Number: number, VersionID: versionID, Result: res}
	if err != nil {
		return out, err
	}
	e, found, err := w.verdict(ctx, id, opID, res)
	if err != nil || !found {
		return out, err
	}
	out.Number = e.Number
	return out, w.refused(ctx, id, e)
}

// versionFor decides the version a publish makes, in the snapshot: its files
// (a rollback's carried from the version it restores), its retention list, and
// every refusal the applier would make, made here first.
func versionFor(ctx context.Context, tx *sql.Tx, head Head, in PublishInput,
	limits artifacts.Limits, versionID string, at time.Time, pins bool) (VersionPayload, error) {

	id := head.ID.String()
	m, contract, caps := in.Manifest, in.Contract, in.Capabilities
	brand := ""
	if pins && in.RestoredFrom == 0 {
		var err error
		if brand, err = currentCSSSha(ctx, tx); err != nil {
			return VersionPayload{}, err
		}
	}
	if in.RestoredFrom != 0 {
		from, found, err := readVersionTx(ctx, tx, id, in.RestoredFrom)
		if err != nil {
			return VersionPayload{}, err
		}
		if !found {
			return VersionPayload{}, fmt.Errorf("%w: %s: this artifact doesn't keep "+
				"version %d", ErrNotFound, id, in.RestoredFrom)
		}
		m = from.Manifest
		for i := range m.Files {
			m.Files[i].Carried = true
		}
		if contract == "" {
			contract = from.Contract
		}
		if caps == nil {
			caps = from.Capabilities
		}
		// A ROLLBACK RESTORES THE PAGE AS IT WAS SERVED, its stylesheet
		// included: the version it restores is retained, so the adoption it
		// pinned is still served.
		if pins {
			brand = from.Brand
		}
	}
	m = manifest.Canonical(m)
	if err := manifest.Validate(head.Kind, m); err != nil {
		return VersionPayload{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := checkUploads(m, in.Uploaded, at); err != nil {
		return VersionPayload{}, err
	}
	if in.Label != "" {
		holder, err := labelHolder(ctx, tx, id, in.Label)
		if err != nil {
			return VersionPayload{}, err
		}
		if holder != 0 {
			return VersionPayload{}, fmt.Errorf("%w: version %d is labelled %q",
				ErrLabelTaken, holder, in.Label)
		}
		count, err := labelled(ctx, tx, id)
		if err != nil {
			return VersionPayload{}, err
		}
		if count >= artifacts.MaxLabels {
			return VersionPayload{}, labelsFull(count)
		}
	}
	prune, err := retention(ctx, tx, id, in.Label == "", 0, head.HeadNumber)
	if err != nil {
		return VersionPayload{}, err
	}
	retired, ok, err := retiredEntries(ctx, tx, id, prune, head.HeadNumber+1, nil)
	if err != nil {
		return VersionPayload{}, err
	}
	if !ok {
		return VersionPayload{}, fmt.Errorf("artifactsdomain: %s's retention list %v "+
			"names a version it cannot retire", id, prune)
	}
	plan, refusal, err := planObjects(ctx, tx, id, m.Files, retired)
	if err != nil {
		return VersionPayload{}, err
	}
	if refusal != nil {
		return VersionPayload{}, planRefusal(refusal)
	}
	if err := checkQuota(ctx, tx, limits, 0, plan.added-plan.freed); err != nil {
		return VersionPayload{}, err
	}
	return VersionPayload{
		Number: head.HeadNumber + 1, VersionID: versionID,
		Base: head.HeadNumber, Forced: in.Force, RestoredFrom: in.RestoredFrom,
		Label: in.Label, Contract: contract, Capabilities: canonicalCapabilities(caps),
		Manifest: m, Prune: prune, SearchText: in.SearchText, Brand: brand,
	}, nil
}

// Patch changes the head's displayed fields or one version's label. A patch
// that changes nothing publishes nothing and answers success.
func (w *Writer) Patch(ctx context.Context, c Call, id artifacts.ID, p Patch) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if p.Title == nil && p.Description == nil && p.Icon == nil && p.Label == nil {
		return Written{}, fmt.Errorf("%w: a patch names at least one field", ErrInvalid)
	}
	if err := checkHead(p.Title, p.Description, p.Icon, true); err != nil {
		return Written{}, err
	}
	if p.Label != nil {
		if p.Label.Number <= 0 {
			return Written{}, fmt.Errorf("%w: a label names its version", ErrInvalid)
		}
		if err := checkText("the label", p.Label.Label, artifacts.MaxLabelBytes, false); err != nil {
			return Written{}, err
		}
	}
	at := w.now()
	opID, err := w.operation(c, string(OpPatch), id.String(), p)
	if err != nil {
		return Written{}, err
	}
	subject, scope := ArtifactSubject(id), ScopeSet{Subject: true}
	if p.Label != nil {
		// A LABEL'S REMOVAL can retire versions, which may pin the brand.
		kind, err := w.kindOf(ctx, id) //nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err != nil {
			return Written{}, err
		}
		if namesBrand(kind) {
			scope = withBrand(subject, scope)
		}
	}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Status == StatusTrashed {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
			}
			payload, changed, err := patchFor(ctx, tx, head, p)
			if err != nil || !changed {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, subject, OpPatch, scope, opID, payload, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// labelsFull is the refusal of a label for a version of an artifact that
// already keeps count labelled versions ([ErrLabelsFull]), naming the remedy a
// person and a model both take: remove a label from a version that no longer
// needs one.
func labelsFull(count int) error {
	return fmt.Errorf("%w: it keeps %d. Remove a label from a version that no "+
		"longer needs one, then label this one", ErrLabelsFull, count)
}

// patchFor is the record a patch publishes, carrying only what it changes, and
// false when it changes nothing.
func patchFor(ctx context.Context, tx *sql.Tx, head Head, p Patch) (PatchPayload, bool, error) {
	out := PatchPayload{V: baseRecordVersion}
	changed := false
	if p.Title != nil && *p.Title != head.Title {
		out.Title, changed = p.Title, true
	}
	if p.Description != nil && *p.Description != head.Description {
		out.Description, changed = p.Description, true
	}
	if p.Icon != nil && *p.Icon != head.Icon {
		out.Icon, changed = p.Icon, true
	}
	if p.Label == nil {
		return out, changed, nil
	}
	id := head.ID.String()
	v, found, err := readVersionTx(ctx, tx, id, p.Label.Number)
	if err != nil {
		return PatchPayload{}, false, err
	}
	if !found {
		return PatchPayload{}, false, fmt.Errorf("%w: %s: this artifact doesn't keep "+
			"version %d", ErrNotFound, id, p.Label.Number)
	}
	if v.Label == p.Label.Label {
		return out, changed, nil
	}
	if p.Label.Label != "" {
		holder, err := labelHolder(ctx, tx, id, p.Label.Label)
		if err != nil {
			return PatchPayload{}, false, err
		}
		if holder != 0 && holder != p.Label.Number {
			return PatchPayload{}, false, fmt.Errorf("%w: version %d is labelled %q",
				ErrLabelTaken, holder, p.Label.Label)
		}
		if v.Label == "" {
			count, err := labelled(ctx, tx, id)
			if err != nil {
				return PatchPayload{}, false, err
			}
			if count >= artifacts.MaxLabels {
				return PatchPayload{}, false, labelsFull(count)
			}
		}
	} else {
		// A LABEL'S REMOVAL makes its version an unlabelled one, so the
		// same record retires whatever that pushes past the kept count.
		prune, err := retention(ctx, tx, id, false, p.Label.Number, head.HeadNumber)
		if err != nil {
			return PatchPayload{}, false, err
		}
		out.Prune = prune
	}
	label := *p.Label
	out.Label = &label
	return out, true, nil
}

// Share grants one audience a role. A share never grants above the sharer's own
// role, and an agent never shares with the whole company.
func (w *Writer) Share(ctx context.Context, c Call, id artifacts.ID, s artifacts.Share) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := checkShare(c, s, c.Authority.Role); err != nil {
		return Written{}, err
	}
	at := w.now()
	grant := artifacts.Share{Audience: s.Audience, Role: s.Role, GrantedBy: c.Authority.Actor, GrantedAt: at}
	opID, err := w.operation(c, string(OpShare), id.String(),
		struct {
			Audience string
			Role     artifacts.Role
		}{s.Audience.String(), s.Role})
	if err != nil {
		return Written{}, err
	}
	subject, scope := ArtifactSubject(id), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Status == StatusTrashed {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
			}
			if s.Audience.Kind == artifacts.AudienceSeat && s.Audience.ID == head.Owner {
				return statelog.Decision{}, fmt.Errorf("%w: %s owns the artifact and holds "+
					"every role on it already", ErrInvalid, head.Owner)
			}
			role, held, err := shareRole(ctx, tx, id.String(), s.Audience)
			if err != nil || (held && role == s.Role) {
				return statelog.Decision{}, err
			}
			// A NEW AUDIENCE IS HELD TO THE GRANT RULE; one already stored is
			// the owner's to change whatever it names, so a share written
			// before the rule is never a record nobody can act on.
			if !held {
				if err := s.Audience.ValidateGrant(); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
					return statelog.Decision{}, fmt.Errorf("%w: %w", ErrInvalid, err)
				}
			}
			return w.decision(stamp, c, subject, OpShare, scope, opID,
				SharePayload{V: baseRecordVersion, Share: grant}, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// Unshare withdraws one audience's share. Withdrawing a share that is not there
// publishes nothing and answers success.
func (w *Writer) Unshare(ctx context.Context, c Call, id artifacts.ID, a artifacts.Audience) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := a.Validate(); err != nil {
		return Written{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	at := w.now()
	opID, err := w.operation(c, string(OpUnshare), id.String(), a.String())
	if err != nil {
		return Written{}, err
	}
	subject, scope := ArtifactSubject(id), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := mustHead(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			_, held, err := shareRole(ctx, tx, id.String(), a) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil || !held {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, subject, OpUnshare, scope, opID,
				UnsharePayload{V: baseRecordVersion, Audience: a}, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// Transfer moves ownership to another seat, whose kind the running chart says.
// The previous owner's source address is released.
func (w *Writer) Transfer(ctx context.Context, c Call, id artifacts.ID, toSeat string) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := checkName("the new owner", toSeat); err != nil {
		return Written{}, err
	}
	kind, err := w.seatKind(ctx, toSeat)
	if err != nil {
		return Written{}, fmt.Errorf("artifactsdomain: resolve the new owner %s: %w", toSeat, err)
	}
	if kind != iam.ActorAgent && kind != iam.ActorHuman {
		return Written{}, fmt.Errorf("%w: %s is a %s, and ownership moves to a seat",
			ErrInvalid, toSeat, kind)
	}
	at := w.now()
	opID, err := w.operation(c, string(OpTransfer), id.String(), toSeat)
	if err != nil {
		return Written{}, err
	}
	subject := ArtifactSubject(id)
	res, err := w.onSourced(ctx, id, func(sourceID string) statelog.Request {
		scope := sourcedScope(id, sourceID)
		return request(subject, scope, opID, statelog.PatternArbitrated,
			func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, err := mustHead(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
				if err != nil {
					return statelog.Decision{}, err
				}
				if err := sameSource(head, sourceID); err != nil {
					return statelog.Decision{}, err
				}
				if head.Status == StatusTrashed {
					return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
				}
				if head.Owner == toSeat {
					return statelog.Decision{}, nil
				}
				return w.decision(stamp, c, subject, OpTransfer, scope, opID,
					TransferPayload{V: baseRecordVersion, To: toSeat, ToKind: kind}, at)
			})
	})
	return w.written(ctx, id, opID, res, err)
}

// LinkWorkItem links an artifact that already exists to one tracker work item,
// so the item's listing finds it ([ListQuery.WorkItem]) exactly as it finds one
// created or published for the item. The record changes no artifact state; its
// history row names the item.
//
// IDEMPOTENT FROM THE CALLER'S VIEW, in both of the ways a caller repeats it. A
// retry under the same key is the same operation, answered from the ledger. A
// second link to an item the artifact's history already names (an earlier
// link, or a create or a publish made for it) publishes nothing and answers
// success: the artifact is on the item's listing already, and a history that
// recorded every repeat would say the link happened twice.
//
// WHO MAY LINK, AND WHETHER THE ITEM EXISTS, ARE DECIDED BEFORE THIS: the
// caller's read of the artifact and their write of the item are the API's
// (internal/api/artifactsapi), and the item is the tracker's, another log this
// writer does not read. The item is stamped on the call's authority; a call
// already charged to another item is refused rather than silently relinked.
func (w *Writer) LinkWorkItem(ctx context.Context, c Call, id artifacts.ID, item string) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	item = strings.TrimSpace(item)
	if err := checkText("the work item", item, artifacts.MaxWorkItemBytes, true); err != nil {
		return Written{}, err
	}
	if charged := c.Authority.WorkItem; charged != "" && charged != item {
		return Written{}, fmt.Errorf("%w: the call is made for work item %s and links %s: "+
			"a link names the one item it is for", ErrInvalid, charged, item)
	}
	authority := c.Authority
	authority.WorkItem = item
	at := w.now()
	opID, err := w.operation(c, string(OpLinkWorkItem), id.String(), item)
	if err != nil {
		return Written{}, err
	}
	subject, scope := ArtifactSubject(id), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Status == StatusTrashed {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
			}
			linked, err := linkedTo(ctx, tx, id.String(), item)
			if err != nil || linked {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, subject, OpLinkWorkItem, scope, opID,
				LinkPayload{V: baseRecordVersion, Authority: authority}, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// linkedTo reports whether artifact id's history already files an applied row
// under work item item, read in the transaction the caller holds: the
// question [ListQuery.WorkItem] asks of the same rows.
func linkedTo(ctx context.Context, tx *sql.Tx, id, item string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_history
		WHERE work_item = ? AND artifact_id = ? AND outcome = ?`,
		item, id, string(OutcomeApplied)).Scan(&n); err != nil {
		return false, fmt.Errorf("artifactsdomain: read whether %s is linked to %s: %w", id, item, err)
	}
	return n > 0, nil
}

// Trash stamps an artifact trashed. Trashing a trashed artifact publishes
// nothing and answers success.
func (w *Writer) Trash(ctx context.Context, c Call, id artifacts.ID) (Written, error) {
	return w.status(ctx, c, id, OpTrash)
}

// Restore takes an artifact out of the trash. Restoring a live artifact
// publishes nothing and answers success.
func (w *Writer) Restore(ctx context.Context, c Call, id artifacts.ID) (Written, error) {
	return w.status(ctx, c, id, OpRestore)
}

// status is a trash or a restore.
func (w *Writer) status(ctx context.Context, c Call, id artifacts.ID, op OpKind) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	at := w.now()
	opID, err := w.operation(c, string(op), id.String(), nil)
	if err != nil {
		return Written{}, err
	}
	subject, scope := ArtifactSubject(id), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if (op == OpTrash) == (head.Status == StatusTrashed) {
				return statelog.Decision{}, nil
			}
			if op == OpRestore && !head.PurgeBegunAt.IsZero() {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrPurging, id)
			}
			return w.decision(stamp, c, subject, op, scope, opID,
				StatusPayload{V: baseRecordVersion}, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// written is a write's answer: its result, and the refusal its own history row
// records where the apply refused it.
func (w *Writer) written(ctx context.Context, id artifacts.ID, opID string, res statelog.Result,
	err error) (Written, error) {

	if err != nil {
		return Written{Result: res}, err
	}
	e, found, err := w.verdict(ctx, id.String(), opID, res)
	if err != nil || !found {
		return Written{Result: res}, err
	}
	return Written{Result: res}, w.refused(ctx, id.String(), e)
}

// Purge removes an artifact permanently, in three steps of one gesture: the
// artifact is committed to its purge on its own subject ([OpPurgeBegin]), every
// dependent's state is removed, and the artifact's own record removes its rows
// and writes the deletion marker. A retry under the same key collapses every
// step that landed and lands the rest.
//
// # Why the purge is committed before anything irreversible
//
// A dependent's removal cannot be undone: the datastore's deletion marker is
// never lifted. A purge judged only before that step and again after it left a
// restore free to land between the two, and a crash there left the artifact for
// anybody to restore, either way a live artifact whose data was gone for good.
// Once the first step has landed, a restore is refused and the artifact is
// never served or written again, so the only way forward is the purge itself:
// its retry, another purge by an administrator, or the trash reaper, which
// finishes every purge that has begun ([Reader.TrashedBefore]).
func (w *Writer) Purge(ctx context.Context, c Call, id artifacts.ID) (Written, error) {
	return w.purge(ctx, c, id, nil)
}

// errNotPurgeable is a purge its guard declined: the artifact is not in the
// state the caller purges from (the trash reaper's, which purges only what has
// been trashed past its term or whose purge has begun).
var errNotPurgeable = errors.New("artifactsdomain: the artifact is not one this purge removes")

// purge is [Writer.Purge] under an optional guard the artifact's head must pass
// before the purge is committed. Once it is (by this gesture or an earlier one),
// the guard is not asked again: a committed purge only ever finishes.
func (w *Writer) purge(ctx context.Context, c Call, id artifacts.ID, guard func(Head) bool) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	at := w.now()
	opID, err := w.operation(c, string(OpPurge), id.String(), nil)
	if err != nil {
		return Written{}, err
	}
	step := statelog.StepOpID(opID, "artifact")
	var (
		gone bool
		kind artifacts.Kind
	)
	if err := w.db.Read(ctx, func(tx *sql.Tx) error { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		head, found, err := readHeadTx(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		switch {
		case err != nil:
			return err
		case found && head.PurgeBegunAt.IsZero() && guard != nil && !guard(head):
			return errNotPurgeable
		case found:
			kind = head.Kind
			return nil
		}
		author, marked, err := deletionMarker(ctx, tx, id.String())
		switch {
		case err != nil:
			return err
		case marked && author == step:
			// THIS GESTURE ALREADY PURGED IT: the retry finishes as the
			// first attempt did.
			gone = true
			return nil
		case marked:
			return fmt.Errorf("%w: %s", ErrPurged, id)
		}
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}); err != nil {
		return Written{}, err
	}
	if !gone {
		if err := w.beginPurge(ctx, c, id, statelog.StepOpID(opID, "begin"), guard, at); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return Written{}, err
		}
	}
	var dependents []string
	if !gone && w.dependents != nil {
		for i, d := range w.dependents() {
			sub := statelog.StepOpID(opID, "dependent-"+strconv.Itoa(i))
			if err := d.PurgeArtifactState(ctx, c, sub, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return Written{}, fmt.Errorf("artifactsdomain: purge %s's dependent state "+
					"(step %d) before the artifact: %w", id, i, err)
			}
			dependents = append(dependents, sub)
		}
	}
	subject := ArtifactSubject(id)
	res, err := w.onSourced(ctx, id, func(sourceID string) statelog.Request {
		scope := sourcedScope(id, sourceID)
		if namesBrand(kind) {
			scope = withBrand(subject, scope)
		}
		return request(subject, scope, step, statelog.PatternArbitrated,
			func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, found, err := readHeadTx(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
				if err != nil {
					return statelog.Decision{}, err
				}
				if !found {
					author, marked, err := deletionMarker(ctx, tx, id.String())
					switch {
					case err != nil:
						return statelog.Decision{}, err
					case marked && author == step:
						return statelog.Decision{}, nil
					case marked:
						return statelog.Decision{}, fmt.Errorf("%w: %s", ErrPurged, id)
					}
					return statelog.Decision{}, fmt.Errorf("%w: %s", ErrNotFound, id)
				}
				if err := sameSource(head, sourceID); err != nil {
					return statelog.Decision{}, err
				}
				if head.PurgeBegunAt.IsZero() {
					// UNREACHABLE after beginPurge landed: the begin is on
					// this subject, and nothing clears it but this record.
					return statelog.Decision{}, fmt.Errorf("artifactsdomain: the purge of %s "+
						"reached its last step with no purge begun on the artifact", id)
				}
				return w.decision(stamp, c, subject, OpPurge, scope, step,
					StatusPayload{V: baseRecordVersion, Dependents: dependents}, at)
			})
	})
	return Written{Result: res}, err
}

// beginPurge is the purge's first step: the artifact committed to its purge on
// its own subject, under the guard where the caller gave one, before any
// dependent's irreversible step. An artifact a purge has already begun on (this
// gesture's earlier attempt, or another purge's) publishes nothing and goes on:
// a begun purge only ever finishes. It answers only once the record is durable,
// so the dependents never run behind a step whose outcome nobody knows.
func (w *Writer) beginPurge(ctx context.Context, c Call, id artifacts.ID, opID string,
	guard func(Head) bool, at time.Time) error {

	subject := ArtifactSubject(id)
	res, err := w.onSourced(ctx, id, func(sourceID string) statelog.Request {
		scope := sourcedScope(id, sourceID)
		return request(subject, scope, opID, statelog.PatternArbitrated,
			func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, err := mustHead(ctx, tx, id.String())
				if err != nil {
					return statelog.Decision{}, err
				}
				if err := sameSource(head, sourceID); err != nil {
					return statelog.Decision{}, err
				}
				if !head.PurgeBegunAt.IsZero() {
					return statelog.Decision{}, nil
				}
				if guard != nil && !guard(head) {
					return statelog.Decision{}, errNotPurgeable
				}
				return w.decision(stamp, c, subject, OpPurgeBegin, scope, opID,
					StatusPayload{V: baseRecordVersion}, at)
			})
	})
	if _, err := w.written(ctx, id, opID, res, err); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return err
	}
	if res.Outcome == statelog.OutcomeUnknown {
		return fmt.Errorf("artifactsdomain: the beginning of %s's purge has an unknown outcome, "+
			"so nothing it would remove was touched; retry the purge under the same key", id)
	}
	return nil
}
