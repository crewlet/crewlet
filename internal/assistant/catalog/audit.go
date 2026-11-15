package catalog

import (
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/provenance"
)

// list_audit_events reads the company's two audit trails, an administrator's:
// the runtime audit of changes made through the tools (GET /events?source=operator)
// and the identity trail (GET /iam/audit).

// The two trails.
const (
	auditActions  = "actions"
	auditIdentity = "identity"
)

// auditPage is a read's default page: the generic read's, which one step holds.
const auditPage = GenericPage

func auditRead() Tool {
	return Tool{
		Name: "list_audit_events", Phrase: "read the audit log",
		Narrow: []string{"by", "failed", "about", "event"},
		Description: "The company's audit trail, newest first. The actions trail (the " +
			"default) is every change made through the engine's tools and who made it: from " +
			"the dashboard, a script, a person's own assistant, or Ask Crewlet once a person " +
			"approved it, with how it ended and why a refused one was refused. The identity " +
			"trail is what happened to people, their sign-ins, credentials and sessions. " +
			"Narrow the actions by who acted or to the failed ones, and the identity trail " +
			"by whom it is about or by one kind of event.",
		schema: object(nil, map[string]*schema{
			"trail": oneOf("Which trail. Default actions.", auditActions, auditIdentity),
			"by": text("Actions: who made the changes, a seat's handle or name, or a login.",
				nameBytes),
			"failed": flag("Actions: only the changes that did not apply (refused or " +
				"failed)."),
			"about": text("Identity: whom the events are about, by their login or id.",
				nameBytes),
			"event": text("Identity: one kind of event, as the trail names it "+
				"(session_opened, credential_issued, ...).", nameBytes),
			"limit": positive("How many records, at most.", 100),
		}),
		Routes: []string{"GET /events", "GET /iam/audit"}, Apps: adminOnly, Fence: true,
		// Actions quote refusals, which can be a vendor's words; the identity
		// trail is the engine's own rows.
		TrustOf: func(a Args, _ any) provenance.Trust {
			if a.String("trail") == auditIdentity {
				return provenance.TrustForHosted("", nil, false)
			}
			return provenance.TrustExternal
		},
		Build: func(a map[string]any) (exec.Request, error) {
			if Args(a).String("trail") == auditIdentity {
				return get("GET /iam/audit", nil, query(a).str("about", "person").
					str("event", "event").num("limit", "limit", auditPage).values())
			}
			q := query(a).fixed("source", "operator").str("by", "actor").
				num("limit", "limit", auditPage)
			if Args(a).Bool("failed") {
				q.fixed("failed", "true")
			}
			return get("GET /events", nil, q.values())
		},
		Variant: by("trail", auditActions),
		Cards: map[string]cards.Spec{
			auditActions: {Table: &cards.Table{Title: "Audit log", Rows: "events",
				More: "next", Brief: []string{"at", "actor", "what"},
				Columns: []cards.Column{col("at", "When", "timestamp", tDatetime),
					seat("actor", "Who", "actor"), prose("what", "What", "summary"),
					col("type", "Record", "type", tLabel),
					col("failed", "Failed", "failed", tText),
					col("id", "Record id", "id", tText)}}},
			auditIdentity: {Table: &cards.Table{Title: "Identity trail", Rows: "events",
				More: "next", Brief: []string{"at", "actor", "what"},
				Columns: []cards.Column{col("at", "When", "at", tDatetime),
					seat("actor", "Who", "actor"), prose("what", "What", "summary"),
					col("event", "Event", "op", tLabel), col("login", "About", "login", tText),
					prose("reason", "Reason", "reason"),
					col("through", "Through", "operator_id", tText)}}},
		},
		Label: func(a map[string]any) string {
			if Args(a).String("trail") == auditIdentity {
				return "Reading the identity trail"
			}
			return "Reading the audit log"
		},
		Scope: func(a Args) string {
			switch {
			case a.String("by") != "":
				return labelled(a, "by", "by")
			case a.String("about") != "":
				return labelled(a, "about", "about")
			case a.Bool("failed"):
				return "failed only"
			}
			return a.String("event")
		},
		Names: map[string]Directory{"by": DirSeat},
	}
}
