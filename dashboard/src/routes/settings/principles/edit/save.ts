/**
 * Saving one part of the charter through the charter's own route
 * (`GET` and `PUT /config/charter`): read the charter as it is now, decide
 * whether the person's draft still answers it, and store it.
 *
 * # The route guards the charter; this guards the part
 *
 * The route's precondition is the CHARTER (its entity-tag is a digest of the
 * three parts), so a seat added or an integration connected meanwhile is no
 * conflict there. A card edits ONE part, though, and a colleague's change to
 * another part is no conflict for it either. So the save keeps the part as
 * the person STARTED from it (the base) and decides on the charter it reads:
 *
 * - the part still reads as the base: nobody else touched it, and the save
 *   writes the charter as it is now with this one part replaced, conditional
 *   on the tag it just read, so whatever a colleague wrote to the other two
 *   parts stays;
 * - the part already reads as the draft: there is nothing to write (a
 *   colleague wrote the same words, or an earlier save whose answer was lost
 *   landed after all);
 * - the part reads as something else: somebody changed it since the person
 *   started, and that is for the PERSON to settle, with what it says now,
 *   never an overwrite and never a silent merge of prose.
 *
 * A charter that moves between this read and this write is refused by the
 * route (`412`) with the charter as it is now, and the save decides again on
 * that, a bounded number of times.
 *
 * # An unanswered write is settled before it is reported
 *
 * A save that got no answer (a gateway that gave up, an engine that failed
 * after storing) may or may not have landed, and reporting either would be a
 * guess. The next read says: the draft is there, so it landed; the base is
 * there, so it did not, and the person is told so; anything else is the
 * conflict it is.
 */

import {
  classifyConfigRefusal,
  type ConfigRefusal,
  type ConfigUnsettled,
} from "~/protocol/configAnswer.ts";
import { answerOf } from "~/protocol/configWrite.ts";
import type { CharterMoved } from "~/contract/config.ts";
import { rest } from "~/protocol/rest.ts";
import type { ActivatedRevision } from "~/lib/nodeStanding.ts";
import {
  charterOf,
  partOf,
  samePart,
  summaryOf,
  withPart,
  type Charter,
  type CharterPart,
  type PartValue,
} from "./charter.ts";

/** Where the charter is read and written. */
export const CHARTER_ROUTE = "/config/charter";

/** The charter as the route answers it, with the tag a write hands back. */
export interface TaggedCharter {
  readonly charter: Charter;
  readonly etag: string;
}

/** What a read of the charter came to. */
export type CharterRead =
  | ({ readonly kind: "charter" } & TaggedCharter)
  /** No company is configured. */
  | { readonly kind: "none" }
  | { readonly kind: "refused"; readonly refusal: ConfigRefusal };

/** What one write of the charter came to. */
export type CharterWrite =
  /**
   * Stored; `activated` is the revision it stored and the epoch that activates
   * at, as the answer names them, and null where it names neither.
   */
  | { readonly kind: "saved"; readonly activated: ActivatedRevision | null }
  /** The charter already said this: nothing was stored. */
  | { readonly kind: "unchanged" }
  /** The charter moved since it was read; this is what it says now. */
  | ({ readonly kind: "moved" } & TaggedCharter)
  | { readonly kind: "none" }
  | { readonly kind: "refused"; readonly refusal: ConfigRefusal };

/** What one save came to. */
export type SaveOutcome =
  /**
   * Stored. `activated` is the revision it stored and the epoch that activates
   * at — what the page follows this node's apply of it by — or null where the
   * save was settled by reading it back after its answer was lost.
   */
  | {
      readonly kind: "saved";
      readonly activated: ActivatedRevision | null;
      readonly summary: string;
    }
  /** The company already holds the draft: nothing was written. */
  | { readonly kind: "unchanged" }
  /** Somebody changed the part since the person started; `theirs` is what it says now. */
  | { readonly kind: "conflict"; readonly theirs: PartValue }
  /** No company is configured to write the charter of. */
  | { readonly kind: "none" }
  /** The engine refused the read or the write. */
  | { readonly kind: "refused"; readonly refusal: ConfigRefusal }
  /**
   * A write whose outcome was unsettled (`refusal`, the last such answer),
   * settled by a read that still shows the base: nothing landed.
   */
  | { readonly kind: "lost"; readonly refusal: ConfigUnsettled }
  /** The charter kept moving under every attempt. */
  | { readonly kind: "busy" };

/** The two requests a save makes, replaceable in a test. */
export interface SaveTransport {
  readonly read: (signal: AbortSignal) => Promise<CharterRead>;
  readonly write: (
    charter: Charter,
    etag: string,
    summary: string,
    signal: AbortSignal,
  ) => Promise<CharterWrite>;
}

/** `GET /config/charter`. Rejects only on an abort. */
export async function readCharter(signal: AbortSignal): Promise<CharterRead> {
  const answer = await answerOf(rest.request("GET", CHARTER_ROUTE, { signal }));
  if (answer.status === 200 && answer.etag) {
    return { kind: "charter", charter: charterOf(record(answer.body)), etag: answer.etag };
  }
  if (answer.status === 404) return { kind: "none" };
  return { kind: "refused", refusal: classifyConfigRefusal(answer) };
}

/** `PUT /config/charter`, conditional on `etag`. Rejects only on an abort. */
export async function writeCharter(
  charter: Charter,
  etag: string,
  summary: string,
  signal: AbortSignal,
): Promise<CharterWrite> {
  const answer = await answerOf(
    rest.request("PUT", CHARTER_ROUTE, {
      headers: { "If-Match": etag, "X-Summary": summary },
      body: charter,
      signal,
    }),
  );
  const body = record(answer.body);
  if (answer.status === 200) {
    if (body.changed === false) return { kind: "unchanged" };
    return {
      kind: "saved",
      activated:
        typeof body.revision_id === "string" && typeof body.epoch === "number"
          ? { revisionId: body.revision_id, epoch: body.epoch }
          : null,
    };
  }
  // THE CHARTER MOVED, and the refusal carries it as it is now with its tag,
  // which is all the next decision needs.
  if (answer.status === 412 && body.error === "version_moved" && typeof body.etag === "string") {
    const moved = body as unknown as CharterMoved;
    return { kind: "moved", charter: charterOf(record(moved.current)), etag: moved.etag };
  }
  if (answer.status === 404) return { kind: "none" };
  const refusal = classifyConfigRefusal(answer);
  return refusal.kind === "conflict" && refusal.reason === "no_active_revision"
    ? { kind: "none" }
    : { kind: "refused", refusal };
}

const ENGINE: SaveTransport = { read: readCharter, write: writeCharter };

/**
 * How many times a save writes before it says the charter kept moving. Each
 * retry follows a refusal because the charter changed in the instant between
 * this save's read and its write; the route already retries a document that
 * moved for any other reason, so three in a row is a charter being rewritten
 * as the person saves.
 */
export const SAVE_ATTEMPTS = 3;

/**
 * Save `draft` as `part`, if the company still holds `base` there.
 * Rejects only on an abort.
 */
export async function savePart(
  {
    part,
    base,
    draft,
  }: { readonly part: CharterPart; readonly base: PartValue; readonly draft: PartValue },
  signal: AbortSignal,
  transport: SaveTransport = ENGINE,
): Promise<SaveOutcome> {
  const read = await transport.read(signal);
  if (read.kind !== "charter") return read;
  let now: TaggedCharter = read;
  let unsettled: ConfigUnsettled | null = null;
  for (let attempt = 0; attempt < SAVE_ATTEMPTS; attempt++) {
    const stored = partOf(now.charter, part);
    if (samePart(stored, draft)) {
      return unsettled
        ? { kind: "saved", activated: null, summary: summaryOf(part, base, draft) }
        : { kind: "unchanged" };
    }
    if (!samePart(stored, base)) return { kind: "conflict", theirs: stored };
    if (unsettled) return { kind: "lost", refusal: unsettled };

    const summary = summaryOf(part, stored, draft);
    const next = withPart(now.charter, part, draft);
    const outcome = await transport.write(next, now.etag, summary, signal);
    switch (outcome.kind) {
      case "saved":
        return { kind: "saved", activated: outcome.activated, summary };
      case "unchanged":
        return { kind: "unchanged" };
      case "moved":
        // DECIDED AGAIN on the charter as it is now: a colleague's change to
        // another part is written around, and one to this part is the
        // person's to settle.
        now = outcome;
        continue;
      case "none":
        return outcome;
      case "refused": {
        if (outcome.refusal.kind === "conflict") return { kind: "busy" };
        if (outcome.refusal.kind !== "unsettled") return outcome;
        // SETTLED BY A READ rather than reported as a guess.
        const settle = await transport.read(signal);
        if (settle.kind !== "charter") return settle;
        now = settle;
        unsettled = outcome.refusal;
        attempt--;
        continue;
      }
    }
  }
  return { kind: "busy" };
}

function record(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}
