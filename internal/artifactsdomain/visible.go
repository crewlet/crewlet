package artifactsdomain

import (
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHO MAY SEE A ROW, AS SQL: the one predicate a listing, its total, a
// folder's counts and the folder tree are each filtered by.
//
// # One predicate, many readers
//
// The authority table decides who may read an artifact or see a folder
// ([authz.Visible], a relation on the object: its owner, a share naming the
// reader's seat, a unit they sit in or the company, or leading the agent that
// owns it). A listing cannot ask it row by row over a whole company, so the
// estate filters by the same relations in SQL first and the surface still
// decides every row it shows. Every count the estate answers beside a page (the
// listing's total, a folder's artifacts, the artifacts in no folder) comes from
// THIS predicate in the same transaction, so a count never holds a row the
// listing beside it could not have shown: a count built from a second
// predicate is the one that drifts, and a drifted count tells a reader about
// rows nobody showed them.
//
// # The same rule as the authority table, exactly
//
// A share counts only when its role is on the ladder ([artifacts.Roles]): the
// authority table ranks a role this build does not know at zero, so a share a
// newer peer wrote with one grants nothing there, and a listing that counted
// it would answer "1 shared with you" over an empty page. An audience kind this
// build does not know matches no arm below, for the same reason.

// relation names the columns a "the viewer holds a relation to this object"
// predicate reads: the object's owner and the owner's kind, its id, and the
// table its shares are kept in with the column naming the object there.
type relation struct {
	owner, ownerKind, id string
	shares, sharesOf     string
}

// artifactRelation is an artifact's own relations, over the alias `a`.
var artifactRelation = relation{
	owner: "a.owner", ownerKind: "a.owner_kind", id: "a.id",
	shares: "artifact_shares", sharesOf: "artifact_id",
}

// rootRelation is a folder's relations, which are its TOP-LEVEL folder's (the
// alias `r`): its owner and its shares decide who sees every folder beneath it
// (ADR-0047).
var rootRelation = relation{
	owner: "r.owner", ownerKind: "r.owner_kind", id: "r.id",
	shares: "artifact_folder_shares", sharesOf: "folder_id",
}

// sharedClause is "shared with the viewer": their seat, a unit they sit in or
// below, or the company, at a role on the ladder.
//
// THEIR SEAT, NOT THEIR RECORD. The record is the seat when the viewer holds
// one and the login when they do not ([iam.RecordOwner]), and a seat share
// names a seat ([artifacts.Audience.Validate]); the authority table matches
// it against the principal's seat alone. Matched against the record, a share
// to a login was counted in the viewer's Shared listing and then withheld by
// every read of it: "2 shared with you" over an empty list. A record in the
// seat grammar is the seat, since a login never is one.
func sharedClause(v Viewer, rel relation) (string, []any) {
	clause := `EXISTS (SELECT 1 FROM ` + rel.shares + ` s WHERE s.` + rel.sharesOf + ` = ` +
		rel.id + ` AND s.role IN (` + placeholders(len(artifacts.Roles)) + `) AND (
		s.audience_kind = ?`
	args := make([]any, 0, len(artifacts.Roles)+5+len(v.Units))
	for _, r := range artifacts.Roles {
		args = append(args, string(r))
	}
	args = append(args, string(artifacts.AudienceCompany))
	if iam.ValidSeatHandle(v.Record) {
		clause += ` OR (s.audience_kind = ? AND s.audience_id = ?)`
		args = append(args, string(artifacts.AudienceSeat), v.Record)
	}
	if len(v.Units) > 0 {
		clause += ` OR (s.audience_kind = ? AND s.audience_id IN (` + placeholders(len(v.Units)) + `))`
		args = append(args, string(artifacts.AudienceUnit))
		for _, u := range v.Units {
			args = append(args, u)
		}
	}
	return clause + `))`, args
}

// relatedClause is "the viewer holds a relation to the object": they own it,
// it is shared with them ([sharedClause]), or they lead the agent that owns it
// (the lead arm, D12, which reaches only an agent owner's objects). For the
// admin path ([Viewer.Admin]) it admits every row.
func relatedClause(v Viewer, rel relation) (string, []any) {
	if v.Admin {
		return `1 = 1`, nil
	}
	shared, args := sharedClause(v, rel)
	clause := `(` + shared + ` OR ` + rel.owner + ` = ?`
	args = append(args, v.Record)
	if len(v.LedOwners) > 0 {
		clause += ` OR (` + rel.ownerKind + ` = ? AND ` + rel.owner + ` IN (` +
			placeholders(len(v.LedOwners)) + `))`
		args = append(args, string(iam.ActorAgent))
		for _, o := range v.LedOwners {
			args = append(args, o)
		}
	}
	return clause + `)`, args
}

// visibleClause is "the viewer may see this artifact" (the alias `a`): THE
// listing's, its total's and every folder count's one predicate.
func visibleClause(v Viewer) (string, []any) { return relatedClause(v, artifactRelation) }

// folderVisibleClause is "the viewer may see this folder": a relation to its
// top-level folder (the alias `r`).
func folderVisibleClause(v Viewer) (string, []any) { return relatedClause(v, rootRelation) }

// everyClause is a predicate holding for every viewer named, ANDed: an agent
// and the requester it acts for each see the row, or neither is shown it.
func everyClause(viewers []Viewer, clause func(Viewer) (string, []any)) (string, []any) {
	var (
		out  string
		args []any
	)
	for i, v := range viewers {
		c, a := clause(v)
		if i > 0 {
			out += ` AND `
		}
		out += c
		args = append(args, a...)
	}
	return out, args
}
