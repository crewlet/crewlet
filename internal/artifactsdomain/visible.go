package artifactsdomain

import (
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHO MAY SEE A ROW, AS SQL: the one predicate a listing, its total, a
// folder's counts and the folder tree are each filtered by. It applies the
// authority table's relations ([authz.Visible]) in SQL, since a listing cannot
// ask that row by row over a company; the surface still decides every row it
// shows. Every count beside a page comes from THIS predicate in the same
// transaction: a count built from a second predicate drifts, and tells a
// reader about rows nobody showed them.
//
// A share counts only at a role on the ladder ([artifacts.Roles]), and an
// audience kind this build does not know matches no arm: the authority table
// grants nothing for either, so counting one would answer "1 shared with you"
// over an empty page.

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
// THEIR SEAT, NOT THEIR RECORD: the record is the login when the viewer holds
// no seat ([iam.RecordOwner]), a seat share names a seat
// ([artifacts.Audience.Validate]), and the authority table matches the
// principal's seat alone; matching the record would count a share to a login
// that every read then withholds. A record in the seat grammar is the seat,
// since a login never is one.
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
