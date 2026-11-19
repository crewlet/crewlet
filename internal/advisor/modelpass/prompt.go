package modelpass

import (
	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// system is what the run is told it is for and how its answer is checked.
const system = `You are the Advisor of a company of AI agents and the people who direct them.
Read the company's last 7 days with the read tools, calling the ones you need together in your first round (each round
sends everything read so far again), then call submit_findings once with the problems you find that
cost the company time, tokens or waiting, and that the advisor's own rules (rule_findings) do not already list.

How each finding is checked before anyone sees it, so write it to pass:
- Cite at least one record (an item key, a turn id or a change) that a tool answered, and at least one number,
  with "from" naming the tool and the path into its answer, such as open_work.blockers[0].blocks. A record or a
  number no tool answered drops the finding.
- Never repeat a rule finding: one about the same subject citing only its records is dropped.
- Offer only a fix the engine builds: round_cap or token_budget for an agent seat, auxiliary_model with a
  providers.llm entry, open_item with an item key you cite, open_seat with an agent's handle, or none.
- Title at most 120 characters, why at most 600, plain sentences, no links or markup. Severity is costs or note.
- Leave out what people dismissed in past_answers. Submit an empty list if you find nothing.

Before submit_findings, if findings_to_explain lists any, call submit_explanations once with one to three plain
sentences for each, by its id: why it matters to this company and what to look at first. An explanation may say
only numbers that finding's own facts, title, why or item say, and only the records it cites; one that says any
other number or record is not kept.`

// opening is the run's first messages.
func opening() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: "Read the company's last 7 days and submit what you find."},
	}
}

// submitDescription is the submission tool's description.
const submitDescription = "Submit the findings, each citing the records and numbers it rests on. " +
	"The engine checks every one and lists only those it can verify."

// explainDescription is the explanation tool's description.
const explainDescription = "Submit an explanation of each finding findings_to_explain lists, by its id. " +
	"The engine keeps only those that cite that finding's own numbers and records."

// explainSchema is the explanations' shape.
var explainSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"explanations"},
	"properties": map[string]any{"explanations": map[string]any{
		"type": "array", "maxItems": advisor.MaxExplained,
		"items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"id", "text"},
			"properties": map[string]any{
				"id":   map[string]any{"type": "string"},
				"text": map[string]any{"type": "string", "maxLength": advisor.MaxExplanationBytes},
			},
		},
	}},
}

// submitSchema is the submission's shape, [advisor.Submitted] for each finding.
var submitSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"findings"},
	"properties": map[string]any{"findings": map[string]any{
		"type": "array", "maxItems": advisor.MaxAnalystFindings,
		"items": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"title", "why", "severity", "subject", "facts", "records", "fix"},
			"properties": map[string]any{
				"title":    map[string]any{"type": "string", "maxLength": advisor.MaxAnalystTitle},
				"why":      map[string]any{"type": "string", "maxLength": advisor.MaxAnalystWhy},
				"item":     map[string]any{"type": "string", "maxLength": advisor.MaxAnalystTitle},
				"severity": map[string]any{"enum": []string{string(advisor.SeverityCosts), string(advisor.SeverityNote)}},
				"subject": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind"},
					"properties": map[string]any{
						"kind": map[string]any{"enum": []string{string(advisor.SubjectCompany),
							string(advisor.SubjectSeat), string(advisor.SubjectRepository)}},
						"seat": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}}},
				"facts": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"key", "value", "from"},
					"properties": map[string]any{"key": map[string]any{"type": "string"},
						"value": map[string]any{"type": "integer"}, "from": map[string]any{"type": "string"}}}},
				"records": map[string]any{"type": "array", "minItems": 1, "maxItems": advisor.MaxAnalystRecords,
					"items": map[string]any{"type": "object", "additionalProperties": false,
						"required": []string{"kind", "id"}, "properties": map[string]any{
							"kind": map[string]any{"enum": []string{string(advisor.RecordItem),
								string(advisor.RecordTurn), string(advisor.RecordChange)}},
							"id": map[string]any{"type": "string"}}}},
				"fix": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"builder"},
					"properties": map[string]any{
						"builder": map[string]any{"enum": builderNames()},
						"seat":    map[string]any{"type": "string"}, "period": map[string]any{"enum": []string{"day", "week", "month"}},
						"value": map[string]any{"type": "integer"}, "entry": map[string]any{"type": "string"},
						"target": map[string]any{"type": "string"}}},
			},
		},
	}},
}

func builderNames() []string {
	var out []string
	for _, b := range advisor.FixBuilders() {
		out = append(out, string(b))
	}
	return out
}
