/**
 * A new active revision of the company document: the one way the demo
 * company's configuration changes, whoever changes it (an org edit through
 * `/config`, a connect through `/setup`, a scenario helper).
 *
 * It does what an apply does to the parts of the company the document
 * decides: the epoch advances, the agent seats follow the roster the new
 * document derives (a seat that joined starting idle, a seat that left taking
 * its live state with it), and the integrations loop runs the pass an apply
 * brings forward (`Worker.MarkStale`). The caller pushes what an apply pushes.
 */

import type { CompanyDocument } from "~/protocol/types.ts";
import type { DemoCompany, DemoSeat } from "./company.ts";
import { derive, mintIdentities } from "./derive.ts";
import { uuidV7 } from "./ids.ts";
import { afterRevision } from "./loop.ts";
import { actorOf } from "./principal.ts";
import { blankSeat } from "./seats.ts";

export interface RevisionOptions {
  /** When it was stored and activated, RFC 3339. */
  at: string;
  /** Which surface wrote it: `api` for `/config`, `setup` for a connect. */
  source: string;
  /** Its id; a fresh UUIDv7 unless a fixture wants a stable one. */
  id?: string;
}

/**
 * Store `doc` as the active revision at the next epoch, and answer its id.
 * Every unit's key and seat's handle it leaves out is minted first, as every
 * engine write does before it stores a document.
 */
export function appendRevision(
  c: DemoCompany,
  doc: CompanyDocument,
  summary: string,
  options: RevisionOptions,
): string {
  const id = options.id ?? uuidV7();
  mintIdentities(doc);
  const author = actorOf(c);
  c.config.revisions.push({
    meta: {
      revision_id: id,
      parent_revision_id: c.config.revisions[c.config.revisions.length - 1]?.meta.revision_id ?? "",
      summary,
      source: options.source,
      created_by: author.name,
      created_by_kind: author.kind,
      operator_id: author.operator_id,
      created_at: options.at,
      activated_at: options.at,
    },
    payload: doc,
  });
  c.config.epoch += 1;
  const agents = (derive(doc).seats ?? []).filter((s) => s.kind === "agent").map((s) => s.handle);
  const kept: Record<string, DemoSeat> = {};
  for (const handle of agents) kept[handle] = c.seats[handle] ?? blankSeat(c, handle, options.at);
  c.seats = kept;
  afterRevision(c, options.at);
  return id;
}

/**
 * Activate the active revision again at the next epoch, and answer its id:
 * what the setup surface does when a submission changed only sealed values,
 * so every running reader of them takes the new ones (`ConfigWriter.Reload`).
 */
export function reloadRevision(c: DemoCompany, at: string): string {
  c.config.epoch += 1;
  afterRevision(c, at);
  return c.config.revisions[c.config.revisions.length - 1]?.meta.revision_id ?? "";
}
