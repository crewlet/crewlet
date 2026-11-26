package googleplay

import (
	"slices"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// The curated tools, for the acts agents make most, each with typed
// arguments; the generic tools reach every other method.
const (
	toolListApps      = "google_play_list_apps"
	toolListReviews   = "google_play_list_reviews"
	toolReadListing   = "google_play_read_listing"
	toolReadTracks    = "google_play_read_tracks"
	toolQueryVitals   = "google_play_query_vitals"
	toolReadReport    = "google_play_read_report"
	toolHaltRelease   = "google_play_halt_release"
	toolReply         = "google_play_reply_to_reviews"
	toolUpdateListing = "google_play_update_listing"
	toolUpdateImages  = "google_play_update_images"
	toolRelease       = "google_play_release"
	toolRead          = "google_play_read"
	toolOperate       = "google_play_operate"
	toolPublish       = "google_play_publish"
	toolSpend         = "google_play_spend"
	toolAdmin         = "google_play_admin"
)

// genericTools are the generic tools by class.
func genericTools() map[growth.Class]string {
	return map[growth.Class]string{
		growth.ClassRead: toolRead, growth.ClassOperate: toolOperate, growth.ClassPublic: toolPublish,
		growth.ClassSpend: toolSpend, growth.ClassAdmin: toolAdmin,
	}
}

// appArg is an app's package name, which every app-level tool names.
func appArg() map[string]any {
	return map[string]any{"type": "string", "description": "The app's package name, one of this agent's apps " +
		"(google_play_list_apps)."}
}

// reasonArg is the one line an administrator reads beside a held act.
func reasonArg() map[string]any {
	return map[string]any{"type": "string", "maxLength": 500,
		"description": "One line saying why, which the administrator reads before answering."}
}

// stringMap is an object of strings.
func stringMap(desc string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
		"description": desc}
}

// stringsSchema is an array of strings.
func stringsSchema(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// mediaArg names the file an upload sends.
func mediaArg() map[string]any {
	return map[string]any{"type": "string", "description": "For an upload: the file to send, as " +
		"artifact:<artifact id>/<path in it> (an asset or a file of the artifact's live version). Nothing else " +
		"is uploaded."}
}

// stepSchema is one staged change of an edit.
func stepSchema(ids []string) map[string]any {
	return toolschema.Object(map[string]any{
		"operation": map[string]any{"type": "string", "enum": ids, "description": "The staged method's ID."},
		"path":      stringMap("Its path parameters but editId, which the engine fills."),
		"query":     stringMap("Its query parameters."),
		"body":      map[string]any{"type": "object", "description": "Its request body."},
		"media":     mediaArg(),
	}, "operation")
}

// toolDefs are every tool, each in one class.
func toolDefs(ops []growth.Operation) []growth.Tool {
	staged := stagedIDs(ops)
	out := []growth.Tool{
		{Name: toolListApps, Class: growth.ClassRead, Operations: []string{"playdeveloperreporting.apps.search"},
			Description: "List the Google Play apps this agent may reach, with each app's package name and title, " +
				"and the ones of its list its identity is not invited to yet.",
			Parameters: toolschema.Object(map[string]any{})},
		{Name: toolListReviews, Class: growth.ClassRead, Operations: []string{"androidpublisher.reviews.list"},
			Description: "List an app's reviews with text, newest first: Google Play answers only reviews written " +
				"or changed in the last 7 days (older ones are in the monthly reviews report).",
			Parameters: toolschema.Object(map[string]any{
				"app": appArg(),
				"translation_language": map[string]any{"type": "string",
					"description": "A language to translate reviews into, such as en."},
				"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 100,
					"description": "Reviews per page, 100 at most."},
				"page_token": map[string]any{"type": "string", "description": "The previous answer's next_page_token."},
			}, "app")},
		{Name: toolReadListing, Class: growth.ClassRead,
			Operations: []string{"androidpublisher.edits.details.get", "androidpublisher.edits.listings.list",
				"androidpublisher.edits.images.list"},
			Description: "Read an app's store listing in every language and its contact details, and with " +
				"image_type the images of one language and type.",
			Parameters: toolschema.Object(map[string]any{
				"app":        appArg(),
				"language":   map[string]any{"type": "string", "description": "For images: the language, such as en-US."},
				"image_type": map[string]any{"type": "string", "enum": imageTypes(), "description": "For images: their type."},
			}, "app")},
		{Name: toolReadTracks, Class: growth.ClassRead, Operations: []string{"androidpublisher.edits.tracks.list"},
			Description: "Read an app's tracks (production, testing, form factors) and each release on them: " +
				"version codes, status, rollout fraction, release notes.",
			Parameters: toolschema.Object(map[string]any{"app": appArg()}, "app")},
		{Name: toolQueryVitals, Class: growth.ClassRead, Operations: vitalsQueries(ops),
			Description: "Query an app's Android vitals over a time range: crash, ANR, slow start, slow rendering, " +
				"wakeups, wakelocks, memory and error counts, by day or hour, with dimensions.",
			Parameters: toolschema.Object(map[string]any{
				"app":        appArg(),
				"metric_set": map[string]any{"type": "string", "enum": metricSets(), "description": "The metric set."},
				"metrics":    stringsSchema("Metric names of the set, such as crashRate and distinctUsers."),
				"dimensions": stringsSchema("Dimension names, such as versionCode, countryCode or deviceModel."),
				"aggregation": map[string]any{"type": "string", "enum": []string{"DAILY", "HOURLY"},
					"description": "DAILY (the default) or HOURLY (crash, ANR and error counts only)."},
				"start_date": map[string]any{"type": "string", "description": "First day, YYYY-MM-DD, in Pacific time."},
				"end_date":   map[string]any{"type": "string", "description": "Day after the last, YYYY-MM-DD."},
				"filter":     map[string]any{"type": "string", "description": "An AIP-160 filter on dimensions."},
				"page_size": map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows,
					"description": "Rows, 200 by default."},
				"page_token": map[string]any{"type": "string"},
			}, "app", "metric_set", "metrics")},
		{Name: toolReadReport, Class: growth.ClassRead,
			Operations: []string{"storage.objects.list", "storage.objects.get"},
			Description: "Read Google Play's monthly reports (installs, ratings, crashes, store performance, " +
				"reviews, and for an agent at administer sales and earnings) from the account's reports bucket, as the company's figures: list them " +
				"by prefix, or read one, its rows decoded.",
			Parameters: toolschema.Object(map[string]any{
				"bucket": map[string]any{"type": "string", "description": "The reports bucket, as " +
					"integrations.google_play.reports_buckets names it; the only one where one is named."},
				"prefix": map[string]any{"type": "string", "description": "To list: a prefix such as stats/installs/."},
				"object": map[string]any{"type": "string", "description": "To read: the report's name, such as " +
					"stats/installs/installs_com.example.app_202611_overview.csv."},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows,
					"description": "Rows to return, 200 by default."},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Rows to skip."},
			})},
		{Name: toolHaltRelease, Class: growth.ClassOperate, Operations: []string{"androidpublisher.edits.tracks.patch"},
			Description: "Halt an app's staged rollout on a track, which stops serving it to more users: made at " +
				"once and recorded. Resuming it is google_play_release, asked first.",
			Parameters: toolschema.Object(map[string]any{
				"app":   appArg(),
				"track": map[string]any{"type": "string", "description": "The track, such as production or beta."},
				"version_codes": stringsSchema("The release's version codes; the track's one release rolling " +
					"out where omitted."),
			}, "app", "track")},
		{Name: toolReply, Class: growth.ClassPublic, Operations: []string{"androidpublisher.reviews.reply"},
			Description: "Ask an administrator to approve replies to 1 to 10 reviews of one app, each at most " +
				"350 characters, published on the store page as shown; replying again replaces a reply.",
			Parameters: toolschema.Object(map[string]any{
				"app": appArg(),
				"replies": map[string]any{"type": "array", "minItems": 1, "maxItems": growth.MaxPublicActs,
					"items": toolschema.Object(map[string]any{
						"review": map[string]any{"type": "string", "description": "The review's ID."},
						"text":   map[string]any{"type": "string", "maxLength": replyLimit, "description": "The reply."},
					}, "review", "text")},
				"reason": reasonArg(),
			}, "app", "replies", "reason")},
		{Name: toolUpdateListing, Class: growth.ClassPublic,
			Operations: []string{"androidpublisher.edits.listings.patch", "androidpublisher.edits.details.patch",
				"androidpublisher.edits.commit"},
			Description: "Ask an administrator to approve a store listing change, published in one commit: one " +
				"language's title, short and full description or video, and the app's contact details.",
			Parameters: toolschema.Object(map[string]any{
				"app":      appArg(),
				"language": map[string]any{"type": "string", "description": "The listing's language, such as en-US."},
				"title": map[string]any{"type": "string", "maxLength": titleLimit,
					"description": "The app's name on the store, 30 characters at most."},
				"short_description": map[string]any{"type": "string", "maxLength": shortLimit},
				"full_description":  map[string]any{"type": "string", "maxLength": fullLimit},
				"video":             map[string]any{"type": "string", "description": "A YouTube video URL."},
				"contact_email":     map[string]any{"type": "string"},
				"contact_phone":     map[string]any{"type": "string"},
				"contact_website":   map[string]any{"type": "string"},
				"default_language":  map[string]any{"type": "string"},
				"reason":            reasonArg(),
			}, "app", "reason")},
		{Name: toolUpdateImages, Class: growth.ClassPublic,
			Operations: []string{"androidpublisher.edits.images.upload", "androidpublisher.edits.images.delete",
				"androidpublisher.edits.images.deleteall", "androidpublisher.edits.commit"},
			Description: "Ask an administrator to approve store images of one language and type, published in " +
				"one commit: upload screenshots or graphics from an artifact, and remove images.",
			Parameters: toolschema.Object(map[string]any{
				"app":        appArg(),
				"language":   map[string]any{"type": "string", "description": "The listing's language, such as en-US."},
				"image_type": map[string]any{"type": "string", "enum": imageTypes()},
				"upload":     stringsSchema("Files to upload, each artifact:<artifact id>/<path in it>, in order."),
				"remove":     stringsSchema("Image IDs to remove (google_play_read_listing lists them)."),
				"remove_all": map[string]any{"type": "boolean", "description": "Remove every image of the type first."},
				"reason":     reasonArg(),
			}, "app", "language", "image_type", "reason")},
		{Name: toolRelease, Class: growth.ClassPublic,
			Operations: []string{"androidpublisher.edits.tracks.patch", "androidpublisher.edits.commit"},
			Description: "Ask an administrator to approve a release on a track, committed in one edit: roll out " +
				"version codes Expo or a build uploaded (to a fraction of users or all), raise a rollout's " +
				"fraction, complete it, resume a halted one, or change a live release's notes or countries.",
			Parameters: toolschema.Object(map[string]any{
				"app":           appArg(),
				"track":         map[string]any{"type": "string", "description": "The track, such as production, beta or internal."},
				"version_codes": stringsSchema("The release's version codes."),
				"status": map[string]any{"type": "string", "enum": []string{"inProgress", "completed"},
					"description": "inProgress for a staged rollout at user_fraction, completed for everyone."},
				"user_fraction": map[string]any{"type": "number", "minimum": 0, "maximum": 1,
					"description": "For inProgress: the share of users, such as 0.1."},
				"name": map[string]any{"type": "string", "description": "The release's name."},
				"release_notes": map[string]any{"type": "array", "items": toolschema.Object(map[string]any{
					"language": map[string]any{"type": "string"}, "text": map[string]any{"type": "string", "maxLength": notesLimit},
				}, "language", "text")},
				"countries":              stringsSchema("For a production rollout: the countries it reaches, ISO 3166 codes."),
				"in_app_update_priority": map[string]any{"type": "integer", "minimum": 0, "maximum": 5},
				"reason":                 reasonArg(),
			}, "app", "track", "version_codes", "status", "reason")},
	}
	for _, class := range []growth.Class{growth.ClassRead, growth.ClassOperate, growth.ClassPublic,
		growth.ClassSpend, growth.ClassAdmin} {
		var ids []string
		for _, op := range ops {
			if comesTo(op, class) && agentCallable(op) && (class != growth.ClassRead || readOnly(op)) {
				ids = append(ids, op.ID)
			}
		}
		props := map[string]any{
			"operation": map[string]any{"type": "string", "enum": ids,
				"description": "The method's ID, as docs/integrations/google-play.md lists it."},
			"path": stringMap("The path's parameters by name, such as {\"packageName\": \"com.example.app\", " +
				"\"language\": \"en-US\"}. The engine opens and fills each edit itself: never name editId."),
			"query": stringMap("Query parameters, such as updateMask or pageToken."),
			"body":  map[string]any{"type": "object", "description": "The request body, as the method takes it."},
		}
		steps := "With androidpublisher.edits.commit: the changes staged in one edit and committed together."
		if class == growth.ClassRead {
			steps = "With androidpublisher.edits.validate: changes staged in an edit the engine validates and " +
				"discards, to check them before asking; no upload."
		} else {
			props["media"] = mediaArg()
		}
		props["steps"] = map[string]any{"type": "array", "minItems": 1, "maxItems": maxSteps,
			"items": stepSchema(staged), "description": steps}
		required := []string{"operation"}
		if class.Held() {
			props["reason"] = reasonArg()
			required = append(required, "reason")
		}
		out = append(out, growth.Tool{Name: genericTools()[class], Class: class, Operations: ids,
			Description: genericDescription(class), Parameters: toolschema.Object(props, required...)})
	}
	return out
}

// genericDescription says what a generic tool of class does.
func genericDescription(class growth.Class) string {
	switch class {
	case growth.ClassRead:
		return "Call any Google Play method that only reads, by its ID, on this agent's apps; a method inside " +
			"an edit is read in one the engine opens and discards."
	case growth.ClassOperate:
		return "Call any reversible Google Play change by its method's ID: draft releases, halted rollouts, " +
			"tester groups, draft products and offers, mapping files. Recorded in the access log; nobody is asked."
	case growth.ClassPublic:
		return "Ask an administrator to approve a Google Play change customers or testers see, by its method's " +
			"ID: listings, images, releases, prices, offers, product states, Data safety, app recovery. Held " +
			"until they answer, at a fresh step-up."
	case growth.ClassSpend:
		return "Ask an administrator to approve a Google Play refund: an order refunded, a subscription revoked " +
			"with a refund, a refund request approved. Held until they answer, at a fresh step-up."
	default:
		return "Ask an administrator to approve a Google Play account, user, access, key, tax or irreversible " +
			"change, by its method's ID: users and grants, app signing, deletions, purchase fulfilment, " +
			"subscription cancels and deferrals. Held until they answer, at a fresh step-up."
	}
}

// comesTo reports whether a call of op can be of class.
func comesTo(op growth.Operation, class growth.Class) bool {
	if op.Class == class {
		return true
	}
	return slices.ContainsFunc(op.Fields, func(r growth.FieldRule) bool { return r.Class == class }) ||
		slices.ContainsFunc(op.States, func(s growth.StateRule) bool { return s.Class == class })
}

// readOnly reports whether every call of op reads, which a read tool needs.
func readOnly(op growth.Operation) bool {
	return op.Class == growth.ClassRead && len(op.Fields) == 0 && len(op.States) == 0
}

// agentCallable reports whether an agent may name op: never rows and the edit
// session's own steps are the engine's (package doc, "Edits are the engine's").
func agentCallable(op growth.Operation) bool {
	return op.Class.Callable() && !slices.Contains(sessionOps(), op.ID)
}

// sessionOps are the edit methods the engine makes around every staged change.
func sessionOps() []string {
	return []string{"androidpublisher.edits.insert", "androidpublisher.edits.get", "androidpublisher.edits.delete"}
}

// stagedIDs are every method staged inside an edit, which a commit's steps
// may name.
func stagedIDs(ops []growth.Operation) []string {
	var out []string
	for _, op := range ops {
		if staged(op) && op.Class != growth.ClassRead {
			out = append(out, op.ID)
		}
	}
	return out
}

// imageTypes are Play's listing image types.
func imageTypes() []string {
	return []string{"phoneScreenshots", "sevenInchScreenshots", "tenInchScreenshots", "tvScreenshots",
		"wearScreenshots", "icon", "featureGraphic", "tvBanner"}
}
