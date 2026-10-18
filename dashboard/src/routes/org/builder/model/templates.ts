/**
 * The shapes a new company can start from.
 *
 * A TEMPLATE IS A STARTING DRAFT, NOT A RULE. It becomes one `applyTemplate`
 * operation holding every seat and unit it creates, with keys minted by the
 * event handler that asked for it, so the operator undoes it in one step and
 * the dry run judges the result like any other draft.
 *
 * FOUR PROMISES, each tested:
 *
 * - ONE REPORTING ROOT. Every template seat except the top one is listed by
 *   handle in exactly one `manages`, and nothing else would give it a manager:
 *   an entry never names a UNIT (which would claim that unit's members as
 *   well, and the first-listed rule would then pick a different manager), and
 *   every unit's only direct member is its own lead (so a lead's automatic
 *   management adds nobody). The chart a new company starts with is therefore
 *   one tree, rather than a root per unit, which is what a template that only
 *   set unit leads produced: the engine's lead manages that unit's direct
 *   members and never the lead of a child unit.
 * - NEVER AN INVENTED CONTACT. A contact identity is how another system — a
 *   chat vendor, a code host, an issue tracker — finds the person a human
 *   seat is, and a made-up one would mention a stranger there. The only
 *   identity a template writes is the one the operator typed for their own
 *   seat, if any; a "Leads are people" lead is created without one. The
 *   engine admits that, and where it costs something — a company talking on
 *   Mattermost or Slack, where nothing addressed to the seat there reaches
 *   anybody — its own check warns at the seat's `contact`, which the review
 *   lists with every other warning. On the engine's own chat an agent
 *   mentions a person by their seat, and nothing is missing.
 * - NEUTRAL TITLES. Seats are named for the role ("Chief Executive"), never a
 *   person, because a seat outlives whoever holds it.
 * - EVERY NODE CARRIES ITS IDENTITY. Each seat is given a handle and each unit
 *   a key minted from its name (`identity.ts`), free of every other, and every
 *   `lead` and `manages` entry names them: the operator's own seat and a
 *   template seat may share a name, and never a handle.
 */

import type { HumanContactKey } from "~/protocol/index.ts";
import type { DraftSeat, DraftUnit } from "./draft.ts";
import { mintHandle, mintUnitKey } from "./identity.ts";
import { mintKey, type KeySource } from "./keys.ts";
import type { Intent, TemplateId } from "./operations.ts";

/** The identities a create form offers for the operator's own seat, in the order it lists them. */
export const CONTACT_IDENTITIES: readonly {
  readonly key: HumanContactKey;
  readonly label: string;
}[] = [
  { key: "slack_user_id", label: "Slack member ID" },
  { key: "mattermost_user_id", label: "Mattermost username" },
  { key: "atlassian_account_id", label: "Atlassian account ID" },
  { key: "github_login", label: "GitHub login" },
  { key: "gitlab_username", label: "GitLab username" },
];

/** Who leads the units of the Established company template. */
export type LeadsAre = "people" | "agents";

/** The operator's own seat, from "Your seat" in the create form. */
export interface FounderSeat {
  readonly name: string;
  /** At most one contact identity: `value` empty for none. */
  readonly identity: HumanContactKey;
  readonly value: string;
}

export interface TemplateOptions {
  readonly template: TemplateId;
  /** The company's name. Its principles are written on Company › Principles once it exists. */
  readonly name: string;
  readonly founder?: FounderSeat;
  /** Required by the Established company template, ignored by the others. */
  readonly leads?: LeadsAre;
}

export type TemplateBuilt =
  { readonly ok: true; readonly intent: Intent } | { readonly ok: false; readonly message: string };

/** One seat of a template, before its handle and key are minted. */
interface SeatSpec {
  readonly name: string;
  readonly kind: "agent" | "human";
  readonly goal: string;
  /** The names of the template seats it manages, each unique within its template. */
  readonly manages?: readonly string[];
}

interface UnitSpec {
  readonly name: string;
  readonly type: string;
  readonly purpose: string;
  /** The unit's lead, who is also its only direct member. */
  readonly lead: SeatSpec;
  readonly children?: readonly UnitSpec[];
}

interface Shape {
  /** The top seat, which the founder manages when there is one. */
  readonly top?: SeatSpec;
  readonly units: readonly UnitSpec[];
}

const CHIEF_EXECUTIVE = "Chief Executive";

function newCompany(): Shape {
  const unit = (name: string, purpose: string, seat: string, goal: string): UnitSpec => ({
    name,
    type: "department",
    purpose,
    lead: { name: seat, kind: "agent", goal },
  });
  const units = [
    unit(
      "Engineering",
      "Build and run the product.",
      "Software Engineer",
      "Design, build and maintain the product's software.",
    ),
    unit(
      "Product",
      "Decide what to build and why.",
      "Product Manager",
      "Shape the roadmap from customer needs and company goals.",
    ),
    unit(
      "Marketing",
      "Tell the market what the company offers.",
      "Content Strategist",
      "Plan and write the content that explains the product.",
    ),
  ];
  return {
    top: {
      name: CHIEF_EXECUTIVE,
      kind: "agent",
      goal: "Set the company's direction and coordinate the work of every unit.",
      manages: units.map((u) => u.lead.name),
    },
    units,
  };
}

function establishedCompany(leads: LeadsAre): Shape {
  const kind = leads === "people" ? "human" : "agent";
  const lead = (name: string, goal: string, manages?: string[]): SeatSpec => ({
    name,
    kind,
    goal,
    ...(manages ? { manages } : {}),
  });
  const units: UnitSpec[] = [
    {
      name: "Engineering",
      type: "department",
      purpose: "Build and run the product.",
      lead: lead("Engineering Lead", "Lead engineering and keep delivery on track.", [
        "Reliability Lead",
      ]),
      children: [
        {
          name: "Reliability",
          type: "team",
          purpose: "Keep the product available, observable and recoverable.",
          lead: lead("Reliability Lead", "Lead reliability work across the product."),
        },
      ],
    },
    {
      name: "Product",
      type: "department",
      purpose: "Decide what to build and why.",
      lead: lead("Product Lead", "Own the roadmap and the decisions behind it.", ["Design Lead"]),
      children: [
        {
          name: "Design",
          type: "team",
          purpose: "Shape how the product looks and works.",
          lead: lead("Design Lead", "Lead product design and research."),
        },
      ],
    },
    {
      name: "Go to Market",
      type: "department",
      purpose: "Bring the product to customers and grow its use.",
      lead: lead("Go to Market Lead", "Lead marketing, sales and customer success."),
    },
  ];
  return {
    top: {
      name: CHIEF_EXECUTIVE,
      kind: "agent",
      goal: "Set the company's direction and coordinate the work of every unit.",
      manages: units.map((u) => u.lead.name),
    },
    units,
  };
}

/**
 * Builds the `applyTemplate` intent for a create form's answers. Call it in
 * the event handler: it mints a key for every node it creates.
 */
export function templateIntent(options: TemplateOptions, keys: KeySource): TemplateBuilt {
  const name = options.name.trim();
  if (name === "") return { ok: false, message: "Enter the company name." };
  const founder = options.founder;
  if (founder && founder.name.trim() === "")
    return { ok: false, message: "Enter a name for your seat, or leave your seat out." };

  let shape: Shape;
  switch (options.template) {
    case "empty":
      shape = { units: [] };
      break;
    case "new_company":
      shape = newCompany();
      break;
    case "established_company":
      if (!options.leads)
        return { ok: false, message: "Choose whether the unit leads are people or agents." };
      shape = establishedCompany(options.leads);
      break;
  }

  // Every identity first, so every reference can name one. The operator's own
  // seat is minted first and keeps the handle its name gives; a template
  // seat that shares its name takes the next free one.
  const taken = new Set<string>();
  const claimHandle = (name: string) => {
    const handle = mintHandle(name, taken);
    taken.add(handle);
    return handle;
  };
  const founderName = founder?.name.trim();
  const founderHandle = founderName ? claimHandle(founderName) : undefined;
  const handles = new Map<string, string>();
  if (shape.top) handles.set(shape.top.name, claimHandle(shape.top.name));
  const unitKeys = new Map<UnitSpec, string>();
  const walkSpecs = (units: readonly UnitSpec[]) => {
    for (const unit of units) {
      handles.set(unit.lead.name, claimHandle(unit.lead.name));
      const key = mintUnitKey(unit.name, taken);
      taken.add(key);
      unitKeys.set(unit, key);
      walkSpecs(unit.children ?? []);
    }
  };
  walkSpecs(shape.units);
  const handleOf = (name: string) => handles.get(name)!;

  const seat = (spec: SeatSpec): DraftSeat => ({
    key: mintKey(keys),
    data: {
      name: spec.name,
      handle: handleOf(spec.name),
      ...(spec.kind === "human" ? { kind: "human" } : {}),
      goal: spec.goal,
      ...(spec.manages && spec.manages.length > 0 ? { manages: spec.manages.map(handleOf) } : {}),
    },
  });
  const unit = (spec: UnitSpec): DraftUnit => ({
    key: mintKey(keys),
    data: {
      name: spec.name,
      id: unitKeys.get(spec)!,
      type: spec.type,
      purpose: spec.purpose,
      lead: handleOf(spec.lead.name),
    },
    roles: [seat(spec.lead)],
    children: (spec.children ?? []).map(unit),
  });

  const roles: DraftSeat[] = [];
  if (founder && founderName && founderHandle) {
    const contact = founder.value.trim();
    roles.push({
      key: mintKey(keys),
      data: {
        name: founderName,
        handle: founderHandle,
        kind: "human",
        ...(contact ? { contact: { [founder.identity]: contact } } : {}),
        ...(shape.top ? { manages: [handleOf(shape.top.name)] } : {}),
      },
    });
  }
  if (shape.top) roles.push(seat(shape.top));

  return {
    ok: true,
    intent: {
      type: "applyTemplate",
      template: options.template,
      name,
      roles,
      units: shape.units.map(unit),
    },
  };
}
