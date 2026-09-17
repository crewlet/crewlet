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
 *   name in exactly one `manages`, and nothing else would give it a manager: an
 *   entry never names a UNIT (which would claim that unit's members as well,
 *   and the first-listed rule would then pick a different manager), and every
 *   unit's only direct member is its own lead (so a lead's automatic
 *   management adds nobody). The chart a new company starts with is therefore
 *   one tree, rather than a root per unit, which is what a template that only
 *   set unit leads produced: the engine's lead manages that unit's direct
 *   members and never the lead of a child unit.
 * - NEVER AN INVENTED IDENTITY. A human seat reaches people through the
 *   contact identity it holds, and a made-up one would mention a stranger. The
 *   only identity a template writes is the one the operator typed for their
 *   own seat; a "Leads are people" lead is created without one, and
 *   [seatsNeedingContact] lists it until the operator supplies it.
 * - NEUTRAL TITLES. Seats are named for the role ("Chief Executive"), never a
 *   person, because a seat's name derives its handle and outlives whoever
 *   holds it.
 * - UNIQUE NAMES. The operator's own seat keeps the name they typed; a
 *   template seat that would collide with it takes the next free name, and
 *   every `manages` entry and lead names the seat as it was finally named.
 */

import type { HumanContactKey } from "~/protocol/index.ts";
import { suggestUniqueName } from "./document.ts";
import { allSeats, type Draft, type DraftSeat, type DraftUnit } from "./draft.ts";
import { mintKey, type KeySource, type NodeKey } from "./keys.ts";
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
  /** Exactly one identity. */
  readonly identity: HumanContactKey;
  readonly value: string;
}

export interface TemplateOptions {
  readonly template: TemplateId;
  readonly charter: { readonly name: string; readonly mission?: string };
  readonly founder?: FounderSeat;
  /** Required by the Established company template, ignored by the others. */
  readonly leads?: LeadsAre;
}

export type TemplateBuilt =
  { readonly ok: true; readonly intent: Intent } | { readonly ok: false; readonly message: string };

/** One seat of a template, before names are made unique and keys minted. */
interface SeatSpec {
  readonly name: string;
  readonly kind: "agent" | "human";
  readonly goal: string;
  /** Seat names, as written before uniqueness. */
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
  const name = options.charter.name.trim();
  if (name === "") return { ok: false, message: "Enter the company name." };
  const founder = options.founder;
  if (founder) {
    if (founder.name.trim() === "")
      return { ok: false, message: "Enter a name for your seat, or leave your seat out." };
    if (founder.value.trim() === "") {
      return {
        ok: false,
        message: "Enter one contact identity for your seat, so agents can reach you.",
      };
    }
  }

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

  // Final names first, so every reference can be rewritten to them.
  const taken = new Set<string>();
  const founderName = founder?.name.trim();
  if (founderName) taken.add(founderName);
  const renamed = new Map<string, string>();
  const claim = (spec: SeatSpec) => {
    const unique = suggestUniqueName(taken, spec.name);
    taken.add(unique);
    renamed.set(spec.name, unique);
  };
  if (shape.top) claim(shape.top);
  const walkSpecs = (units: readonly UnitSpec[]) => {
    for (const unit of units) {
      claim(unit.lead);
      walkSpecs(unit.children ?? []);
    }
  };
  walkSpecs(shape.units);
  const final = (seat: string) => renamed.get(seat) ?? seat;

  const seat = (spec: SeatSpec): DraftSeat => ({
    key: mintKey(keys),
    data: {
      name: final(spec.name),
      ...(spec.kind === "human" ? { kind: "human" } : {}),
      goal: spec.goal,
      ...(spec.manages && spec.manages.length > 0 ? { manages: spec.manages.map(final) } : {}),
    },
  });
  const unit = (spec: UnitSpec): DraftUnit => ({
    key: mintKey(keys),
    data: { name: spec.name, type: spec.type, purpose: spec.purpose, lead: final(spec.lead.name) },
    roles: [seat(spec.lead)],
    children: (spec.children ?? []).map(unit),
  });

  const roles: DraftSeat[] = [];
  if (founder && founderName) {
    roles.push({
      key: mintKey(keys),
      data: {
        name: founderName,
        kind: "human",
        contact: { [founder.identity]: founder.value.trim() },
        ...(shape.top ? { manages: [final(shape.top.name)] } : {}),
      },
    });
  }
  if (shape.top) roles.push(seat(shape.top));
  const mission = options.charter.mission?.trim();

  return {
    ok: true,
    intent: {
      type: "applyTemplate",
      template: options.template,
      charter: { name, ...(mission ? { mission } : {}) },
      roles,
      units: shape.units.map(unit),
    },
  };
}

/**
 * The human seats of a draft that hold no contact identity, in walk order.
 *
 * A checklist for the create flow, not a validator: the engine refuses a human
 * seat with no identity, and this lets the review name each one before the
 * operator finds out from a refused check.
 */
export function seatsNeedingContact(draft: Draft): NodeKey[] {
  const out: NodeKey[] = [];
  for (const { seat } of allSeats(draft)) {
    if (seat.data.kind !== "human") continue;
    const contact = seat.data.contact;
    const hasOne =
      contact !== undefined &&
      Object.values(contact).some((v) => typeof v === "string" && v.trim() !== "");
    if (!hasOne) out.push(seat.key);
  }
  return out;
}
