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

	// THE GALLERY'S FOLDERS AND THE COMPANY'S BRAND.
	nevers(whyFolders, "POST /artifact-folders", "PATCH /artifact-folders/{id}",
		"DELETE /artifact-folders/{id}", "PUT /artifacts/{id}/folder",
		"POST /artifacts/folder-moves"),
	links(whyBrand, "GET /brand", "GET /brand/files/{hash}"),
	nevers("adopting or clearing the brand changes the company's public face, an "+
		"administrator's gesture on its own screen", "POST /brand/adoptions",
		"DELETE /brand/adoptions/current"),

	// THE COMPANY'S PRINCIPLES, the one part of the company document the
	// assistant reaches (ADR-0051's exception): read as the person, and
	// written only as the change its owner approved from a before-and-after.
	[]Exposure{
		routed(PrinciplesRoute, "get_company_principles"),
		routeProposed(PrinciplesWriteRoute, "update_company_principles"),
	},

	// EVERY OTHER COMPANY DOCUMENT ROUTE, each an administrator's or a
	// lead's; an org edit is a page action into the org builder's draft.
	nevers("the company document names every secret reference, and its edits are "+
		"page actions into the editors' drafts that the person saves",
		"GET /config", "OPTIONS /config", "PUT /config", "PATCH /config", "POST /config/reload",
		"GET /config/references",
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
		"GET /secrets", "POST /secrets/rekey", "POST /secrets/reveal", "GET /secrets/{name}",
		"PUT /secrets/{name}", "DELETE /secrets/{name}"),
	nevers("connecting an integration enters credentials and creates vendor apps",
		"GET /setup/integrations", "GET /setup/integrations/{kind}",
		"POST /setup/integrations/{kind}/inputs", "DELETE /setup/integrations/{kind}",
		"POST /setup/integrations/{kind}/verify", "POST /setup/integrations/{kind}/provision",
		"POST /setup/integrations/{kind}/check", "POST /setup/integrations/github/app",
		"GET /setup/integrations/{kind}/runs/{id}", "GET /setup/integrations/{kind}/runs"),
	nevers("the deployment's own controls change what every node does and copy every "+
		"credential; an administrator runs them on Settings",
		"POST /backup", "POST /objects/migrate", "POST /worklet/retention/ack",
		"POST /worklet/retention/evict/{node}", "POST /worklet/retention/readmit/{node}",
		"POST /worklet/retention/capacity", "GET /worklet/retention/maintenance",
		"POST /worklet/retention/maintenance/abandon", "POST /worklet/retention/maintenance/exclude",
		"GET /worklet/retention/reanchor", "POST /worklet/retention/reanchor",
		"POST /fleet/broker/remove/{node}", "POST /fleet/broker/remove-peer/{peer}"),

	// THE HUMAN WRITE SURFACE, for scripts and the CLI: the assistant's
	// writes go through the act route the dashboard's controls send.
	nevers(whyWriteSurface,
		"POST /worklet/items", "PATCH /worklet/items/{key}", "POST /worklet/items/{key}/comments",
		"PATCH /worklet/items/{key}/comments/{cid}", "POST /worklet/items/{key}/rank",
		"POST /worklet/items/{key}/depend", "POST /worklet/items/{key}/relate",
		"DELETE /worklet/items/{key}", "POST /worklet/items/{key}/restore", "POST /worklet/items/{key}/move",
		"PUT /worklet/projects/{key}", "POST /worklet/projects/{key}/tags", "POST /worklet/views",
		"PUT /worklet/people/{handle}/inbox", "PUT /worklet/people/{handle}/pins",
		"PUT /worklet/people/{handle}/priorities", "POST /doclet/pages", "PUT /doclet/pages/{id}",
		"POST /doclet/pages/{id}/comments", "PATCH /doclet/pages/{id}/comments/{cid}",
		"DELETE /doclet/pages/{id}/comments/{cid}", "POST /doclet/pages/{id}/rename", "DELETE /doclet/pages/{id}",
		"POST /doclet/pages/{id}/restore", "PUT /worklet/files/{project}/{path...}",
		"DELETE /worklet/files/{project}/{path...}"),
	nevers(whyIrreversible, "POST /worklet/items/{key}/purge", "POST /doclet/pages/{id}/purge",
		"PUT /worklet/catalogue"),
	links("a project file's bytes are opened from the project's Files lens",
		"GET /worklet/files/{project}/{path...}"),
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
