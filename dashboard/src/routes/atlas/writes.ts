/**
 * What the atlas's write controls send, worked out apart from how they are
 * drawn: how an entry is named to a write, what an Add entry form comes to,
 * which facts and relations a kind admits, and the decisions a waiting item
 * allows. Every table here is read off the engine's vocabulary
 * (`contract/atlas.ts`), so a control offers only what the engine accepts,
 * and what it refuses is still the engine's own sentence.
 */

import {
  ATLAS_FACT_SPECS,
  ATLAS_RELATION_PAIRS,
  type AtlasEdge,
  type AtlasEntry,
  type AtlasFact,
  type AtlasFactView,
  type AtlasKind,
  type AtlasRelation,
  type AtlasReviewItem,
  type AtlasVendor,
} from "~/contract/atlas.ts";
import { KINDS, RELATIONS, factLabel, valueWord } from "./words.ts";

/** An entry as a write names it: its kind and key, which names one member of a merged entry. */
export function entryArg(ref: { kind: AtlasKind; key: string }): string {
  return `${ref.kind}:${ref.key}`;
}

/** What a fact admits, as the engine's vocabulary states it. */
export interface FactSpec {
  type: string;
  traits: readonly string[];
  words: readonly string[];
  kinds: readonly string[];
}

const FACT_SPECS: Readonly<Record<AtlasFact, FactSpec>> = ATLAS_FACT_SPECS;

export function factSpec(fact: AtlasFact): FactSpec {
  return FACT_SPECS[fact];
}

/** The facts an entry of `kind` may carry, in the order a reader is shown them. */
export function factsOf(kind: AtlasKind): AtlasFact[] {
  return (Object.keys(FACT_SPECS) as AtlasFact[]).filter((fact) => {
    const { kinds } = FACT_SPECS[fact];
    return kinds.length === 0 || kinds.includes(kind);
  });
}

const PAIRS: Readonly<
  Record<
    AtlasRelation,
    { from: readonly string[]; to: readonly string[]; externals: readonly string[] }
  >
> = ATLAS_RELATION_PAIRS;

/** The relations an entry of `kind` may draw to another entry, with the kinds each may point at. */
export function relationsFrom(kind: AtlasKind): { relation: AtlasRelation; to: AtlasKind[] }[] {
  return (Object.keys(PAIRS) as AtlasRelation[])
    .filter((relation) => PAIRS[relation].from.includes(kind) && PAIRS[relation].to.length > 0)
    .map((relation) => ({ relation, to: [...PAIRS[relation].to] as AtlasKind[] }));
}

/** Whether an entry of `kind` may belong to a vendor account. */
export function belongs(kind: AtlasKind): boolean {
  return PAIRS.belongs_to.from.includes(kind);
}

/**
 * The kinds a person adds by hand. A model and a tool server are declared in
 * the company's settings and enter the atlas from there, so the form does not
 * offer what the engine would refuse.
 */
export const ADDABLE_KINDS: readonly AtlasKind[] = (Object.keys(KINDS) as AtlasKind[]).filter(
  (kind) => kind !== "model_provider" && kind !== "tool_server",
);

/** How a kind's key is written: the field's helper and an example. */
export const KEY_HINTS: Readonly<Record<AtlasKind, { helper: string; example: string }>> = {
  domain: { helper: "The zone itself, never a wildcard.", example: "acme.example" },
  hostname: { helper: "The full host name.", example: "api.acme.example" },
  endpoint: {
    helper: "The address with its scheme, without credentials or a query.",
    example: "https://api.acme.example/v1",
  },
  certificate: {
    helper: "The vendor, a slash and the vendor's own identifier.",
    example: "cloudflare/7f3a9c",
  },
  service: {
    helper: "The name people use for it: 1 to 40 lower-case letters, digits and hyphens.",
    example: "checkout-api",
  },
  workload: {
    helper: "A namespace, a slash and its identifier.",
    example: "acme/checkout-worker",
  },
  host: { helper: "A namespace, a slash and its identifier.", example: "acme/web-2" },
  datastore: { helper: "A namespace, a slash and its identifier.", example: "acme/orders-db" },
  queue: { helper: "A namespace, a slash and its identifier.", example: "acme/orders" },
  bucket: { helper: "A namespace, a slash and its identifier.", example: "acme/assets" },
  vendor_account: {
    helper:
      "The account's identifier at the vendor, or a short name for one the atlas does not know.",
    example: "acme-cdn",
  },
  repository: { helper: "Its host and its path.", example: "github.com/acme/api" },
  pipeline: {
    helper: "Its repository's key, a hash sign and its name.",
    example: "github.com/acme/api#deploy",
  },
  monitor: { helper: "A namespace, a slash and its identifier.", example: "datadog/api-5xx" },
  tool_server: { helper: "Declared in the company's settings.", example: "mcp/tickets" },
  model_provider: { helper: "Declared in the company's settings.", example: "llm/main" },
};

/** What the Add entry form holds. */
export interface AddDraft {
  kind: AtlasKind;
  key: string;
  name: string;
  /** An account's vendor: part of its key. */
  vendor: AtlasVendor;
  /** The id of the vendor account another kind of entry belongs to, or "". */
  account: string;
  purpose: string;
  owner: { kind: "seat" | "unit"; name: string } | null;
  /** The Doclet page that documents it. */
  page: { id: string; title: string } | null;
}

/** The key an Add entry form names: an account's is its vendor, a slash and what was typed. */
export function draftKey(draft: Pick<AddDraft, "kind" | "key" | "vendor">): string {
  const key = draft.key.trim();
  if (draft.kind !== "vendor_account") return key;
  return key === "" ? draft.vendor : `${draft.vendor}/${key}`;
}

/** The arguments of `atlas_add_entry` a form comes to: only what was filled in. */
export function addArgs(draft: AddDraft): Record<string, string> {
  const args: Record<string, string> = { kind: draft.kind, key: draftKey(draft) };
  const name = draft.name.trim();
  const purpose = draft.purpose.trim();
  if (name !== "") args.name = name;
  if (purpose !== "") args.purpose = purpose;
  if (draft.account !== "" && belongs(draft.kind)) args.belongs_to = draft.account;
  if (draft.owner) args[draft.owner.kind] = draft.owner.name;
  if (draft.page) args.doclet_page = draft.page.id;
  return args;
}

/** The tools a decision is sent through. */
export type DecisionTool =
  | "atlas_confirm"
  | "atlas_correct"
  | "atlas_unrelate"
  | "atlas_relate"
  | "atlas_set_owner"
  | "atlas_retire";

/** One decision a person may take on something that waits for them. */
export interface Decision {
  key: string;
  tool: DecisionTool;
  args: Record<string, string | boolean>;
  /** The button's words. */
  label: string;
  /** What the decision did, for the sentence that reports it. */
  done: string;
  /** The decision accepts what the atlas shows now: what a bulk accept sends. */
  accepts?: boolean;
  /** The decision takes something off the map. */
  removes?: boolean;
}

/** Who made a claim, as far as deciding it goes. */
type Voice = { source: { kind: string } };

/**
 * Why a disagreement is not decided in the atlas, or null where it is. What
 * the company's settings or the engine's own records declare is changed where
 * it is declared: the engine refuses every decision on it here, so no button
 * is drawn that could only be refused.
 */
export function settledElsewhere(shown: Voice, others: readonly Voice[] = []): string | null {
  const kinds = [shown, ...others].map((claim) => claim.source.kind);
  if (kinds.includes("settings")) {
    return "The company’s settings declare this, so it is settled there: change the settings, or what the other source reports.";
  }
  if (kinds.includes("records")) {
    return "The engine’s own records say this, so it is settled where they come from, never in the atlas.";
  }
  return null;
}

/** Why an owner the sources disagree on is not decided by a button: Set owner names the one owner. */
export const OWNER_DECIDED_BY = "An entry has one owner. Name it with Set owner on the entry.";

/** A value as a sentence writes it. */
function said(value: { type: string; text: string }): string {
  return value.type === "word" ? valueWord(value.text) : value.text;
}

/** The decisions a fact its sources disagree on allows: keep the value shown, or take another. */
export function factDecisions(
  entry: Pick<AtlasEntry, "ref" | "name">,
  fact: AtlasFactView,
): Decision[] {
  if (settledElsewhere(fact, fact.others)) return [];
  const subject = entryArg(fact.of ?? entry.ref);
  const label = factLabel(fact);
  const many = factSpec(fact.fact).traits.includes("many");
  const out: Decision[] = [
    {
      key: "keep",
      tool: "atlas_confirm",
      args: { atlas_entry: subject, fact: fact.fact, value: fact.value.text },
      label: `Keep ${said(fact.value)}`,
      done: `Kept ${label} of ${entry.name} as ${said(fact.value)}`,
      accepts: true,
    },
  ];
  const offered = new Set([fact.value.text]);
  for (const other of fact.others ?? []) {
    if (other.denied) {
      if (out.some((one) => one.key === "wrong")) continue;
      out.push({
        key: "wrong",
        tool: "atlas_correct",
        args: {
          atlas_entry: subject,
          fact: fact.fact,
          wrong: true,
          ...(many ? { value: fact.value.text } : {}),
        },
        label: "Say it is wrong",
        done: `Recorded that ${label} of ${entry.name} is not ${said(fact.value)}`,
        removes: true,
      });
    } else if (other.value && !offered.has(other.value.text)) {
      offered.add(other.value.text);
      out.push({
        key: `take:${other.value.text}`,
        tool: "atlas_confirm",
        args: { atlas_entry: subject, fact: fact.fact, value: other.value.text },
        label: `Take ${said(other.value)}`,
        done: `Set ${label} of ${entry.name} to ${said(other.value)}`,
      });
    }
  }
  return out;
}

/** The decisions a relation its sources disagree on allows: keep it, or remove it. */
export function relationDecisions(
  entry: Pick<AtlasEntry, "ref" | "name">,
  edge: AtlasEdge,
  targetName: string,
): Decision[] {
  if (settledElsewhere(edge, edge.others)) return [];
  const subject = entryArg(entry.ref);
  const words = `${RELATIONS[edge.relation].toLowerCase()} ${targetName}`;
  const kept = `Kept that ${entry.name} ${words}`;
  const removed = `Recorded that ${entry.name} no longer ${words}`;
  if (edge.to) {
    const args = { atlas_entry: subject, relation: edge.relation, to: edge.to };
    return [
      { key: "keep", tool: "atlas_confirm", args, label: "Keep it", done: kept, accepts: true },
      {
        key: "remove",
        tool: "atlas_unrelate",
        args,
        label: "Remove it",
        done: removed,
        removes: true,
      },
    ];
  }
  if (edge.external === "page" && edge.name) {
    const args = { atlas_entry: subject, relation: edge.relation, doclet_page: edge.name };
    return [
      { key: "keep", tool: "atlas_relate", args, label: "Keep it", done: kept, accepts: true },
      {
        key: "remove",
        tool: "atlas_unrelate",
        args,
        label: "Remove it",
        done: removed,
        removes: true,
      },
    ];
  }
  // AN OWNER IS NAMED BY SET OWNER, which replaces every owner a person
  // named: a button here could only keep or clear all of them at once.
  return [];
}

/** Why one waiting item carries no decision of its own, or null where it does. */
export function undecided(item: AtlasReviewItem): string | null {
  const claim = item.fact ?? item.relation;
  if (!claim) return null;
  const elsewhere = settledElsewhere(claim, claim.others);
  if (elsewhere) return elsewhere;
  return item.relation?.relation === "owned_by" ? OWNER_DECIDED_BY : null;
}

/** The decisions one waiting item allows, the one that accepts what is shown first. */
export function reviewDecisions(item: AtlasReviewItem): Decision[] {
  const entry = {
    ref: { kind: item.entry.kind, key: item.entry.key },
    name: item.entry.name,
  };
  if (item.fact) return factDecisions(entry, item.fact);
  if (item.relation) return relationDecisions(entry, item.relation, item.target?.name ?? "it");
  const subject = entryArg(entry.ref);
  return [
    {
      key: "confirm",
      tool: "atlas_confirm",
      args: { atlas_entry: subject },
      label: "Confirm",
      done: `Confirmed ${entry.name}`,
      accepts: true,
    },
    {
      key: "retire",
      tool: "atlas_retire",
      args: { atlas_entry: subject },
      label: "Retire",
      done: `Retired ${entry.name}`,
      removes: true,
    },
  ];
}
