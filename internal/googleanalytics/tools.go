package googleanalytics

import (
	"slices"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// The curated tools, for the acts agents make most, each with typed
// arguments; the generic tools reach every other method.
const (
	toolListProperties = "google_analytics_list_properties"
	toolRunReport      = "google_analytics_run_report"
	toolRealtime       = "google_analytics_run_realtime_report"
	toolMarkKeyEvent   = "google_analytics_mark_key_event"
	toolRead           = "google_analytics_read"
	toolOperate        = "google_analytics_operate"
	toolAdmin          = "google_analytics_admin"
)

// genericTools are the generic tools by class.
func genericTools() map[growth.Class]string {
	return map[growth.Class]string{
		growth.ClassRead: toolRead, growth.ClassOperate: toolOperate,
		growth.ClassAdmin: toolAdmin,
	}
}

// propertyArg is a property's ID, which every property-level tool names.
func propertyArg() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[0-9]{1,20}$",
		"description": "The numeric property ID, one of this agent's properties (google_analytics_list_properties)."}
}

// stringList is an array of strings.
func stringList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// toolDefs are every tool, each in one class.
func toolDefs(ops []growth.Operation) []growth.Tool {
	report := toolschema.Object(map[string]any{
		"property":   propertyArg(),
		"start_date": map[string]any{"type": "string", "description": "First day, YYYY-MM-DD, or Google's NdaysAgo, yesterday or today. Default 28daysAgo."},
		"end_date":   map[string]any{"type": "string", "description": "Last day, YYYY-MM-DD, NdaysAgo, yesterday or today. Default yesterday."},
		"dimensions": stringList("Up to 9 dimension API names (date, sessionSource, pagePath ...); google_analytics_read getMetadata lists them."),
		"metrics":    stringList("Up to 10 metric API names (sessions, activeUsers, keyEvents ...)."),
		"dimension_filter": map[string]any{"type": "object",
			"description": "A FilterExpression as the Data API takes it."},
		"metric_filter": map[string]any{"type": "object", "description": "A FilterExpression on metrics."},
		"order_bys":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "OrderBy objects as the Data API takes them."},
		"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows, "description": "Rows to return, 200 by default."},
		"offset":        map[string]any{"type": "integer", "minimum": 0, "description": "Rows to skip, for the next page."},
	}, "property", "metrics")
	realtime := toolschema.Object(map[string]any{
		"property":   propertyArg(),
		"dimensions": stringList("Realtime dimension API names (country, unifiedScreenName ...)."),
		"metrics":    stringList("Realtime metric API names (activeUsers, eventCount ...)."),
		"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows},
	}, "property", "metrics")
	keyEvent := toolschema.Object(map[string]any{
		"property":   propertyArg(),
		"event_name": map[string]any{"type": "string", "description": "The event to mark as a key event."},
		"counting_method": map[string]any{"type": "string", "enum": []string{"ONCE_PER_EVENT", "ONCE_PER_SESSION"},
			"description": "How Google counts it; ONCE_PER_EVENT by default."},
	}, "property", "event_name")
	out := []growth.Tool{
		{Name: toolListProperties, Class: growth.ClassRead,
			Operations:  []string{"analyticsadmin.accountSummaries.list"},
			Description: "List the Google Analytics accounts and properties this agent may reach, with each property's ID and name.",
			Parameters:  toolschema.Object(map[string]any{})},
		{Name: toolRunReport, Class: growth.ClassRead, Operations: []string{"analyticsdata.properties.runReport"},
			Description: "Run a Google Analytics report on one property: dimensions and metrics over a date range, " +
				"with Google's thresholding, sampling and other-row flags passed through.",
			Parameters: report},
		{Name: toolRealtime, Class: growth.ClassRead, Operations: []string{"analyticsdata.properties.runRealtimeReport"},
			Description: "Read a property's last 30 minutes of activity (60 on Analytics 360). Never cached.",
			Parameters:  realtime},
		{Name: toolMarkKeyEvent, Class: growth.ClassOperate, Operations: []string{"analyticsadmin.properties.keyEvents.create"},
			Description: "Mark an event as a key event on a property. It changes reporting only; unmarking is google_analytics_admin's keyEvents.delete.",
			Parameters:  keyEvent},
	}
	for _, class := range []growth.Class{growth.ClassRead, growth.ClassOperate, growth.ClassAdmin} {
		var ids []string
		for _, op := range ops {
			if comesTo(op, class) {
				ids = append(ids, op.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		out = append(out, growth.Tool{
			Name: genericTools()[class], Class: class, Operations: ids,
			Description: genericDescription(class),
			Parameters: toolschema.Object(map[string]any{
				"operation": map[string]any{"type": "string", "enum": ids,
					"description": "The method's ID, as docs/integrations/google-analytics.md lists it."},
				"path": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "The path's parameters by name, such as {\"propertiesId\": \"123\", \"keyEventsId\": \"456\"}."},
				"query": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "Query parameters, such as updateMask or pageToken."},
				"body": map[string]any{"type": "object", "description": "The request body, as the method takes it."},
			}, "operation"),
		})
	}
	return out
}

// genericDescription says what a generic tool of class does.
func genericDescription(class growth.Class) string {
	switch class {
	case growth.ClassRead:
		return "Call any Google Analytics method that only reads, by its ID, on this agent's properties."
	case growth.ClassOperate:
		return "Call any reversible Google Analytics change by its method's ID: settings, definitions, audiences, " +
			"annotations. Recorded in the access log; nobody is asked."
	default:
		return "Ask an administrator to approve a Google Analytics account, access, link, key, billing or irreversible change, " +
			"by its method's ID; held until they answer, at a fresh step-up."
	}
}

// comesTo reports whether a call of op can be of class.
func comesTo(op growth.Operation, class growth.Class) bool {
	if op.Class == class {
		return true
	}
	return slices.ContainsFunc(op.Fields, func(r growth.FieldRule) bool { return r.Class == class || r.Lower == class })
}
