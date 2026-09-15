/**
 * Creating the company on an engine that has none.
 *
 * WHAT IT ASKS FOR is what only a person can answer: the company's name and
 * mission, the shape to start from, and whether the operator wants a seat of
 * their own. Everything else a template writes (units, neutral role titles,
 * who manages whom) is in `model/templates.ts`, and what it produces is one
 * `applyTemplate` operation, so the whole start is a single undo and is
 * checked by the engine exactly like any other draft.
 *
 * NO INVENTED IDENTITY. A human seat reaches people through a contact
 * identity, and a template never writes one it was not given: the only
 * identity here is the one the operator types for their own seat. The
 * "Leads are people" option therefore creates human leads without identities,
 * and the review lists each one until it has one.
 *
 * WHAT A CREATE CANNOT DO. The dashboard writes no model provider (that is
 * `providers.llm`, which no screen edits), so the next steps after a
 * successful create name the two things left to do and give the command for
 * the one that has no screen at all.
 */

import { useState } from "react";
import { href } from "~/app/router.tsx";
import { Checkbox } from "~/ui/Checkbox.tsx";
import { Field } from "~/ui/Field.tsx";
import type { HumanContactKey } from "~/protocol/index.ts";
import type { KeySource } from "./model/keys.ts";
import type { Intent, TemplateId } from "./model/operations.ts";
import { CONTACT_IDENTITIES, templateIntent, type LeadsAre } from "./model/templates.ts";
import { AccountTreeGlyph, CheckGlyph } from "@crewlethq/icons/glyphs";
import { Button, ButtonLink, Callout, Card, CodeBlock, SegmentedControl } from "@crewlethq/ui";
import { RECORD_MAX_HEIGHT } from "~/components/common.tsx";

/** What each starting point gives the operator, in one line. */
const TEMPLATES: readonly { id: TemplateId; label: string; hint: string }[] = [
  {
    id: "new_company",
    label: "New company",
    hint: "A chief executive and three units (Engineering, Product, Marketing), each with one agent seat.",
  },
  {
    id: "established_company",
    label: "Established company",
    hint: "A chief executive, Engineering with a Reliability team, Product with a Design team, and Go to Market.",
  },
  {
    id: "empty",
    label: "Start empty",
    hint: "The charter alone. Add every unit and seat yourself.",
  },
];

export function CreateCompany({
  keys,
  onApply,
  disabled,
}: {
  keys: KeySource;
  /** The template operation to record. */
  onApply: (intent: Intent) => void;
  disabled: boolean;
}) {
  const [name, setName] = useState("");
  const [mission, setMission] = useState("");
  const [template, setTemplate] = useState<TemplateId>("new_company");
  const [leads, setLeads] = useState<LeadsAre>("agents");
  const [ownSeat, setOwnSeat] = useState(false);
  const [seatName, setSeatName] = useState("");
  const [identity, setIdentity] = useState<HumanContactKey>(CONTACT_IDENTITIES[0]!.key);
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);
  const chosen = TEMPLATES.find((t) => t.id === template)!;

  const start = () => {
    const built = templateIntent(
      {
        template,
        charter: { name, ...(mission.trim() ? { mission } : {}) },
        ...(ownSeat ? { founder: { name: seatName, identity, value } } : {}),
        ...(template === "established_company" ? { leads } : {}),
      },
      keys,
    );
    if (!built.ok) {
      setError(built.message);
      return;
    }
    setError(null);
    onApply(built.intent);
  };

  return (
    <Card as="section">
      <Card.Header
        icon={<AccountTreeGlyph size="sm" />}
        subtitle="the charter, a shape to start from, and your own seat"
      >
        <Card.Title>Create the company</Card.Title>
      </Card.Header>
      <div className="col gap-4 measure">
        <Field label="Company name" value={name} onChange={setName} autoFocus disabled={disabled} />
        <Field
          label="Mission"
          kind="multiline"
          rows={2}
          value={mission}
          onChange={setMission}
          disabled={disabled}
          help="What the company is for. Every agent reads it."
        />

        <div className="col gap-1">
          <span className="t-cell">Start from</span>
          <SegmentedControl<TemplateId>
            label="Start from"
            semantics="radio"
            value={template}
            onValueChange={setTemplate}
            options={TEMPLATES.map((t) => ({ value: t.id, label: t.label }))}
          />
          <span className="t-caption">{chosen.hint}</span>
        </div>

        {template === "established_company" && (
          <div className="col gap-1">
            <span className="t-cell">Unit leads</span>
            <SegmentedControl<LeadsAre>
              label="Unit leads"
              semantics="radio"
              value={leads}
              onValueChange={setLeads}
              options={[
                { value: "agents", label: "Leads are agents" },
                { value: "people", label: "Leads are people" },
              ]}
            />
            <span className="t-caption">
              {leads === "people"
                ? "Each unit lead is a human seat. Every one needs a contact identity before the company can be created."
                : "Each unit lead is an agent seat the engine runs."}
            </span>
          </div>
        )}

        <Checkbox
          framed
          label="Add a seat for yourself"
          description="A human seat at the top of the organization, so agents can reach you and escalate to you."
          checked={ownSeat}
          onChange={setOwnSeat}
          disabled={disabled}
        />
        {ownSeat && (
          <div className="col gap-3">
            <Field
              label="Your seat's name"
              value={seatName}
              onChange={setSeatName}
              disabled={disabled}
              help="The seat is named for the role, not the person: it outlives whoever holds it."
            />
            <Field
              label="How agents reach you"
              kind="choice"
              value={identity}
              onChange={(v) => setIdentity(v as HumanContactKey)}
              disabled={disabled}
              choices={CONTACT_IDENTITIES.map((c) => ({ value: c.key, label: c.label }))}
            />
            <Field
              label={CONTACT_IDENTITIES.find((c) => c.key === identity)!.label}
              value={value}
              onChange={setValue}
              disabled={disabled}
            />
          </div>
        )}

        {/* Why Start the company did nothing, announced: the form does not
            move, so a reader who cannot see the paragraph appear is left with
            a button that looks unpressed. */}
        {error && (
          <Callout variant="danger" role="alert">
            {error}
          </Callout>
        )}
        <div className="row gap-1">
          <Button variant="primary" onClick={start} disabled={disabled}>
            Start the company
          </Button>
        </div>
      </div>
    </Card>
  );
}

/**
 * What is left to do once the company exists: connect the tools it works in,
 * and give it a model provider, which no dashboard screen writes.
 */
export function NextSteps({ onDismiss }: { onDismiss: () => void }) {
  return (
    <Card as="section">
      <Card.Header
        icon={<CheckGlyph size="sm" />}
        subtitle="two steps the dashboard cannot take for you"
        actions={
          <Button size="small" variant="tertiary" onClick={onDismiss}>
            Dismiss
          </Button>
        }
      >
        <Card.Title>The company is created</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        <div className="col gap-1">
          <strong>Connect chat and trackers</strong>
          <span className="t-caption">
            Agents reach people and work through the company's integrations.
          </span>
          <div className="row">
            <ButtonLink variant="secondary" size="small" href={href(["integrations"])}>
              Open Integrations
            </ButtonLink>
          </div>
        </div>
        <div className="col gap-1">
          <strong>Add a model provider</strong>
          <span className="t-caption">
            Until one is configured no agent seat takes a turn, and work sent to a seat waits on its
            inbox until it is. No dashboard screen writes{" "}
            <code className="inline">providers.llm</code>. Seal the key first with{" "}
            <code className="inline">crewlet secrets set ANTHROPIC_API_KEY</code>, then either
            import a company file or patch the configuration:
          </span>
          <CodeBlock plain wrap code={PROVIDER_SNIPPET} maxHeight={RECORD_MAX_HEIGHT} />
        </div>
      </div>
    </Card>
  );
}

/** The exact commands the step above names. */
const PROVIDER_SNIPPET = `# Either: edit the company file and import it
crewlet config import company.yaml

# Or: patch the running configuration
curl -X PATCH "$CREWLET_URL/config" \\
  -H "Authorization: Bearer $CREWLET_TOKEN" \\
  -H "Content-Type: application/merge-patch+json" \\
  -H "X-Summary: add a model provider" \\
  -d '{"providers":{"llm":{"default":{"type":"anthropic",
       "model":"claude-sonnet-5","api_keys":["\${ANTHROPIC_API_KEY}"]}}}}'`;
