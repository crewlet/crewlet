/**
 * Creating the company on an engine that has none.
 *
 * WHAT IT ASKS FOR is what only a person can answer: the company's name, the
 * shape to start from, and whether the operator wants a seat of their own. Everything else a template writes (units, neutral role titles,
 * who manages whom) is in `model/templates.ts`, and what it produces is one
 * `applyTemplate` operation, so the whole start is a single undo and is
 * checked by the engine exactly like any other draft.
 *
 * YOUR OWN SEAT IS OFFERED ON, named for the role. Every person holds a human
 * seat for as long as they exist, and is invited or created onto one from
 * the org chart — so a company started with no human seat is one nobody,
 * the operator included, can join until somebody adds one. Unticking it is
 * still an answer (an agents-only company run through API tokens), and a
 * deliberate one.
 *
 * NO INVENTED CONTACT. A human seat reaches people through a contact
 * identity, and a template never writes one it was not given: the only
 * identity here is the one the operator may type for their own seat. The
 * "Leads are people" option therefore creates human leads without identities,
 * which the engine admits, and the review lists each one.
 *
 * NOT ITS PRINCIPLES. The mission, the vision and the standing policies are
 * written on Company › Principles and nowhere else, which needs the company
 * to exist first, so the next steps after a successful create open that page
 * in edit rather than this form asking for one of the three in passing.
 *
 * WHAT A CREATE CANNOT DO. The dashboard writes no model provider (that is
 * `providers.llm`, which no screen adds), so the next steps after a successful
 * create name it too and give the terminal steps for the one step that has no
 * screen at all (`contract/providersteps.ts`) — unless another system manages
 * the document, which is where its model is added then
 * ([managedModelSentence]).
 */

import { useState } from "react";
import { principlesEditHref } from "~/routes/settings/principles/edit/address.ts";
import { href } from "~/app/router.tsx";
import { ConfigField } from "~/components/ConfigField.tsx";
import { PROVIDER_STEPS } from "~/contract/providersteps.ts";
import { managerOf } from "~/protocol/configAnswer.ts";
import type { HumanContactKey } from "~/protocol/index.ts";
import type { KeySource } from "./model/keys.ts";
import type { Intent, TemplateId } from "./model/operations.ts";
import { CONTACT_IDENTITIES, templateIntent, type LeadsAre } from "./model/templates.ts";
import { screenPath } from "./dialogParts.tsx";
import { NetworkGlyph, CheckGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  ButtonLink,
  Callout,
  Card,
  Checkbox,
  CodeBlock,
  InlineCode,
  SegmentedControl,
} from "@crewlethq/ui";

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
    hint: "The company alone. Add every unit and seat yourself.",
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
  const [template, setTemplate] = useState<TemplateId>("new_company");
  const [leads, setLeads] = useState<LeadsAre>("agents");
  // ON, AND NAMED: see the file's doc. A name left empty is refused by the
  // template, so a box ticked from the start starts with one.
  const [ownSeat, setOwnSeat] = useState(true);
  const [seatName, setSeatName] = useState("Founder");
  const [identity, setIdentity] = useState<HumanContactKey>(CONTACT_IDENTITIES[0]!.key);
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);
  const chosen = TEMPLATES.find((t) => t.id === template)!;

  const start = () => {
    const built = templateIntent(
      {
        template,
        name,
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
      <Card.Header icon={<NetworkGlyph size="sm" />}>
        <Card.Title>Create the company</Card.Title>
      </Card.Header>
      <div className="col gap-4 measure">
        <ConfigField
          label="Company name"
          value={name}
          onChange={setName}
          autoFocus
          disabled={disabled}
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
                ? "Each team lead is a person, with no contact identity until you add one, so no agent can mention them yet."
                : "Each team lead is an agent."}
            </span>
          </div>
        )}

        <Checkbox
          framed
          label="Add a seat for yourself"
          description="At the top, so agents can reach you. It’s the seat you’ll hold: once the company exists, invite or create yourself on it from the org chart."
          checked={ownSeat}
          onCheckedChange={setOwnSeat}
          disabled={disabled}
        />
        {ownSeat && (
          <div className="col gap-3">
            <ConfigField
              label="Your seat's name"
              value={seatName}
              onChange={setSeatName}
              disabled={disabled}
              help="Name the role, not the person."
            />
            <ConfigField
              label="How agents reach you"
              kind="choice"
              value={identity}
              onChange={(v) => setIdentity(v as HumanContactKey)}
              disabled={disabled}
              choices={CONTACT_IDENTITIES.map((c) => ({ value: c.key, label: c.label }))}
            />
            <ConfigField
              label={CONTACT_IDENTITIES.find((c) => c.key === identity)!.label}
              value={value}
              onChange={setValue}
              required={false}
              disabled={disabled}
              help="Leave it empty if you will work only through the dashboard: no agent can then mention you."
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
 * Where a managed company's model is added: where the rest of the company is
 * written (ADR-0030).
 *
 * THE TERMINAL STEPS ARE NOT DRAWN FOR ONE, whoever reads it, a writer the
 * engine names included: they end in an offline `crewlet config import`, which
 * a managed document refuses to every caller (`cmd/crewlet/managed.go`), and
 * the managing system replaces any revision written around it at its next
 * reconcile. What the document is managed by is the engine's answer, so the
 * sentence names it rather than guessing at a tool.
 */
export function managedModelSentence(managedBy: readonly string[]): string {
  return `The company’s configuration is managed by ${managerOf(managedBy)}, so the model is added there.`;
}

/**
 * What is left to do once the company exists: write the principles every
 * agent reads, connect the tools it works in, and give it a model provider,
 * which no dashboard screen adds. The company
 * this card follows is in the store and in no file, which is where the steps
 * start (`contract/providersteps.ts` says why each one is the one it is). On a
 * document another system manages, `managedBy` names it, and the card says the
 * model is added there instead ([managedModelSentence]).
 */
export function NextSteps({
  onDismiss,
  managedBy,
}: {
  onDismiss: () => void;
  managedBy: readonly string[] | null;
}) {
  return (
    <Card as="section">
      <Card.Header
        icon={<CheckGlyph size="sm" />}
        subtitle="three steps left"
        actions={
          <Button size="small" variant="ghost" onClick={onDismiss}>
            Dismiss
          </Button>
        }
      >
        <Card.Title>The company is created</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        <div className="col gap-1">
          <strong>Write its principles</strong>
          <span className="t-caption">
            The mission, the vision and the standing policies every agent reads on every turn.
          </span>
          <div className="row">
            <ButtonLink variant="secondary" size="small" href={principlesEditHref()}>
              Open Principles
            </ButtonLink>
          </div>
        </div>
        <div className="col gap-1">
          <strong>Connect chat and trackers</strong>
          <div className="row">
            <ButtonLink variant="secondary" size="small" href={href(screenPath("integrations"))}>
              Open Integrations
            </ButtonLink>
          </div>
        </div>
        <div className="col gap-1">
          <strong>Add a model provider</strong>
          {managedBy === null ? (
            <>
              <span className="t-caption">
                Agents can’t work until one is added. Every model runs through a coding CLI, signed
                in on the node’s host. These steps write the company you just created to a file, add
                Claude Code to it, sign it in with <InlineCode>crewlet llm login</InlineCode> and
                import the file:
              </span>
              <CodeBlock plain wrap code={PROVIDER_STEPS} />
            </>
          ) : (
            <span className="t-caption">
              Agents can’t work until one is added. {managedModelSentence(managedBy)}
            </span>
          )}
        </div>
      </div>
    </Card>
  );
}
