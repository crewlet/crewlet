package catalog

import "slices"

// routeExposures are the guarded routes' (each mounted with its verb through
// authz.Router, internal/api's route table keeping both) and the overview
// reads the stream service serves. Every guarded route has its row whatever
// its verb, so a route added to any surface is a decision somebody makes; the
// walk refuses anything but Never for a route whose verb the authority table
// keeps from the assistant.
var routeExposures = slices.Concat(
	[]Exposure{
		routed("GET /agents", "list_agents"),
		routed("GET /org", "get_org_chart"),
		routeNever("GET /tools", "the tool registry's overview: catalogue internals no question "+
			"needs"),
		routeNever("GET /stream/snapshot", "the whole handshake bundle, unbounded; the specific "+
			"reads cover its parts"),
		routed("GET /iam/seats", "get_org_chart"),
		routeProposed(ShareRoute, "share_artifact"),
		routeProposed(UnshareRoute, "unshare_artifact"),
	},

	// THE ARTIFACTS SURFACE. Every route is admitted on the gallery's verb
	// and decides its own inside, so the row is the decision about the
	// gesture, not about the verb it is mounted on.
	nevers(whyAuthor,
		"POST /artifacts/uploads", "POST /artifacts/{id}/uploads",
		"PUT /artifacts/uploads/{upload}/files/{path...}", "POST /artifacts",
		"POST /artifacts/checks", "POST /artifacts/{id}/versions",
		"POST /artifacts/{id}/versions/{n}/restore", "PATCH /artifacts/{id}",
		"PATCH /artifacts/{id}/versions/{n}", "POST /artifacts/{id}/assets",
		"DELETE /artifacts/{id}/assets/{asset}", "GET /artifacts/starters/slides",
		"GET /artifacts/sources/preview/{name}"),
	nevers(whyIrreversible,
		"DELETE /artifacts/{id}", "POST /artifacts/{id}/restore", "POST /artifacts/{id}/purge",
		"POST /artifacts/{id}/transfer", "PUT /artifacts/{id}/data/schema"),
	nevers(whyLater,
		"PUT /artifacts/{id}/pin", "DELETE /artifacts/{id}/pin",
		"POST /artifacts/{id}/work-items", "POST /artifacts/{id}/data/writes",
		"POST /artifacts/{id}/data/undo", "POST /artifacts/{id}/data/state"),
	nevers("a view ticket is a credential bound to the session that opened the "+
		"frame; the artifact's screen mints its own", "POST /artifacts/{id}/tickets"),
	nevers("the frame's own telemetry, which only an open artifact reports",
		"POST /artifacts/ready"),
	links("an artifact's files, its source and its rendered views are the artifact's "+
		"screen, which a person opens",
		"GET /artifacts/{id}/versions/{n}/files/{path...}", "GET /artifacts/{id}/versions/{n}/source",
		"GET /artifacts/{id}/versions/{n}/deck", "GET /artifacts/{id}/versions/{n}/doc",
		"GET /artifacts/{id}/versions/{n}/outline", "GET /artifacts/{id}/versions/{n}/dashboard",
		"GET /artifacts/{id}/versions/{n}/sources/{dataset}", "GET /artifacts/sources",
		"GET /artifacts/{id}/data/export.csv"),

	// THE COMPANY DOCUMENT, every route an administrator's or a lead's, and
	// org and principles edits are page actions into their editors' drafts.
	nevers("the company document names every secret reference, and its edits are "+
		"page actions into the editors' drafts that the person saves",
		"GET /config", "OPTIONS /config", "PUT /config", "PATCH /config", "POST /config/reload",
		"GET /config/references", "GET /config/charter", "PUT /config/charter",
		"GET /config/revisions", "GET /config/revisions/{id}", "GET /config/revisions/{id}/diff",
		"POST /config/revisions/{id}/revert", "GET /config/roles/{id}", "PUT /config/roles/{id}",
		"GET /config/units/{id}", "PUT /config/units/{id}", "GET /config/llm-providers/{id}",
		"PUT /config/llm-providers/{id}", "GET /config/mcp-servers/{id}",
		"PUT /config/mcp-servers/{id}"),

	// THE IDENTITY DIRECTORY: who can reach the company and how, and every
	// credential gesture, are a person's own at their keyboard.
	nevers("identity and credentials are never the assistant's: who can reach the "+
		"company, and how, is changed and read by a person on its own screen",
		"GET /iam/people", "POST /iam/people", "GET /iam/people/{id}", "PATCH /iam/people/{id}",
		"DELETE /iam/people/{id}", "POST /iam/invitations", "GET /iam/invitations",
		"DELETE /iam/invitations/{id}", "GET /iam/people/{id}/sessions",
		"DELETE /iam/people/{id}/sessions", "POST /iam/people/{id}/mfa/reset",
		"POST /iam/people/{id}/password-reset", "GET /iam/credentials", "POST /iam/credentials",
		"DELETE /iam/credentials/{id}", "POST /iam/invalidate-all", "GET /iam/check",
		"GET /iam/node-tokens", "GET /iam/audit"),

	nevers("the credential store: a listing alone says which credentials a company holds",
		"GET /secrets", "POST /secrets/rekey", "GET /secrets/{name}", "PUT /secrets/{name}",
		"DELETE /secrets/{name}"),
	nevers("connecting an integration enters credentials and creates vendor apps",
		"GET /setup/integrations", "GET /setup/integrations/{kind}",
		"POST /setup/integrations/{kind}/inputs", "DELETE /setup/integrations/{kind}",
		"POST /setup/integrations/{kind}/verify", "POST /setup/integrations/{kind}/provision",
		"POST /setup/integrations/{kind}/check", "POST /setup/integrations/github/app",
		"GET /setup/integrations/{kind}/runs/{id}", "GET /setup/integrations/{kind}/runs"),
	nevers("the deployment's own controls change what every node does and copy every "+
		"credential; an administrator runs them on Settings",
		"POST /backup", "POST /objects/migrate", "POST /work/retention/ack",
		"POST /work/retention/evict/{node}", "POST /work/retention/readmit/{node}",
		"POST /work/retention/capacity", "GET /work/retention/maintenance",
		"POST /work/retention/maintenance/abandon", "POST /work/retention/maintenance/exclude",
		"GET /work/retention/reanchor", "POST /work/retention/reanchor",
		"POST /fleet/broker/remove/{node}", "POST /fleet/broker/remove-peer/{peer}"),

	// THE HUMAN WRITE SURFACE, for scripts and the CLI: the assistant's
	// writes go through the act route the dashboard's controls send.
	nevers(whyWriteSurface,
		"POST /work/items", "PATCH /work/items/{key}", "POST /work/items/{key}/comments",
		"PATCH /work/items/{key}/comments/{cid}", "POST /work/items/{key}/rank",
		"POST /work/items/{key}/depend", "POST /work/items/{key}/relate",
		"DELETE /work/items/{key}", "POST /work/items/{key}/restore", "POST /work/items/{key}/move",
		"PUT /work/projects/{key}", "POST /work/projects/{key}/tags", "POST /work/views",
		"PUT /work/people/{handle}/inbox", "PUT /work/people/{handle}/pins",
		"PUT /work/people/{handle}/priorities", "POST /pages", "PUT /pages/{id}",
		"POST /pages/{id}/comments", "PATCH /pages/{id}/comments/{cid}",
		"DELETE /pages/{id}/comments/{cid}", "POST /pages/{id}/rename", "DELETE /pages/{id}",
		"POST /pages/{id}/restore", "PUT /work/files/{project}/{path...}",
		"DELETE /work/files/{project}/{path...}"),
	nevers(whyIrreversible, "POST /work/items/{key}/purge", "POST /pages/{id}/purge",
		"PUT /work/catalogue"),
	links("a project file's bytes are opened from the project's Files lens",
		"GET /work/files/{project}/{path...}"),
)

// nevers are routes kept from the assistant for one reason.
func nevers(why string, routes ...string) []Exposure {
	out := make([]Exposure, 0, len(routes))
	for _, r := range routes {
		out = append(out, routeNever(r, why))
	}
	return out
}

// links are routes the assistant only links to, for one reason.
func links(why string, routes ...string) []Exposure {
	out := make([]Exposure, 0, len(routes))
	for _, r := range routes {
		out = append(out, routeLinked(r, why))
	}
	return out
}
