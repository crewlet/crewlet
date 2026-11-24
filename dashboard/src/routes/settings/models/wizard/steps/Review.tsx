/**
 * Everything the save makes, as one revision, in words first: who changes
 * model, the model, its sign-in, where it runs, its limits and web tools, and
 * whether the test call ran; on an add, whether it joins the default fleet.
 * Then what the engine said of it, each problem once with the step that fixes
 * it. No engine path is printed (design QA M8, M9).
 *
 * WHAT THE WRITE DROPS IS SAID (decision D3). A field the page writes holds
 * one model; a fallback written there by hand is replaced, and each one is
 * named here before it goes.
 */

import type { ReactNode } from "react";
import {
  Callout,
  ChangeGroups,
  Checkbox,
  DescriptionList,
  Link,
  NoticeList,
  Text,
} from "@crewlethq/ui";
import {
  DEFAULT_HARNESS_MAX_CONCURRENT,
  DEFAULT_HARNESS_TIMEOUT_SECONDS,
} from "~/contract/models.ts";
import { href } from "~/app/router.tsx";
import { listed } from "~/lib/format.ts";
import { SEAT_JOBS, type Dropped, type Uses } from "~/lib/modelUses.ts";
import type { ConfigProblem, ConfigWarning } from "~/protocol/types.ts";
import type { Fleet } from "~/protocol/fleets.ts";
import { HarnessMark, MARK_SIZE } from "../../HarnessMark.tsx";
import type { ModelsData } from "../../data.ts";
import { JOB_WORDS, seatName } from "../../places.ts";
import { RUN_IN } from "../../words.ts";
import {
  CLOUD_FIELDS,
  STEP_LABELS,
  canExecute,
  hasSandbox,
  factsOf,
  fieldOf,
  messageOf,
  modelId,
  fixable,
  stepOf,
  type Draft,
} from "../draft.ts";
import type { WizardStep } from "../../parts.tsx";
import { methodWords } from "./SignInStep.tsx";
import { Waiting } from "../../Waiting.tsx";
import { webToolsWords } from "./LimitsStep.tsx";
import { seatChangesOf } from "../uses.ts";
import { classOf, classPair } from "../../fleets/data.ts";
import { ConfigWarnings } from "~/components/ConfigWarnings.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { catalogueOf, nameIn } from "../../modelNames.ts";

/** The write of a save that is running: the credential's seal, then the document. */
export type SaveStage = "credential" | "document";

/** Where the engine's check of the change stands. */
export type ReviewCheck =
  | { kind: "idle" }
  | { kind: "checking"; what: "credential" | "document" }
  | { kind: "ready"; warnings: readonly ConfigWarning[] }
  /** The engine refused the change on its problems, which the list shows. */
  | { kind: "problems" }
  /** The change cannot be made from here as it stands: a managed document, a conflict, no engine. */
  | { kind: "blocked"; message: string };

/** "QA's reviewing also named sonnet as a fallback; this removes it." */
export function droppedWords(dropped: Dropped, names: (handle: string) => string): string {
  const many = dropped.keys.length > 1;
  return `${names(dropped.seat)}'s ${JOB_WORDS[dropped.job]} also named ${listed(dropped.keys)} as ${
    many ? "fallbacks" : "a fallback"
  }; this removes ${many ? "them" : "it"}.`;
}

/** The default fleet, as the engine reads it; null before the reading arrives. */
export function defaultFleetOf(data: Pick<ModelsData, "fleets">): Fleet | null {
  return data.fleets?.fleets.find((f) => f.is_default) ?? null;
}

/** Whether a model fits a fleet: any class joins one with no member, else its class and harness. */
export function fitsFleet(
  fleet: Pick<Fleet, "members" | "class" | "harness">,
  draft: Pick<Draft, "harness" | "provider" | "model">,
): boolean {
  if (fleet.members.length === 0) return true;
  return fleet.class === classOf(modelId(draft)) && fleet.harness === draft.harness;
}

/** The other models a change leaves with no use at all. */
export function orphansOf(data: Pick<ModelsData, "models">, draft: Draft): string[] {
  return data.models
    .filter((m) => m.key !== draft.key)
    .filter((m) => {
      const jobs = SEAT_JOBS.flatMap((j) => m.uses.jobs[j].map((seat) => [j, seat] as const));
      if (jobs.length === 0 && !m.uses.assistant && !m.uses.advisor) return false;
      const seatsMoved = jobs.every(([j, seat]) => draft.uses.jobs[j].includes(seat));
      return (
        seatsMoved &&
        (!m.uses.assistant || draft.uses.assistant) &&
        (!m.uses.advisor || draft.uses.advisor)
      );
    })
    .map((m) => m.key);
}

export function Review({
  draft,
  data,
  dropped,
  editing,
  served,
  keptCredential,
  problems,
  check,
  saving = false,
  testRow,
  toDefault,
  first = false,
  onToDefault,
  onGo,
}: {
  draft: Draft;
  data: Pick<ModelsData, "models" | "seats" | "assistant" | "advisor" | "fleets"> &
    Partial<Pick<ModelsData, "doc">>;
  dropped: readonly Dropped[];
  editing: boolean;
  /** What the model is used for today: none for a model being added. */
  served: Uses;
  keptCredential: boolean;
  problems: readonly ConfigProblem[];
  check: ReviewCheck;
  /** Which write of a save is running, or false while none is. */
  saving?: SaveStage | false;
  /** The Test call row: its words and, where it has not passed, the way to run it. */
  testRow: ReactNode;
  /** Whether an added model joins the default fleet where it fits: null until the reader chooses. */
  toDefault: boolean | null;
  /** A company's first model, which joins the default fleet with its seats and helpers on it. */
  first?: boolean;
  onToDefault: (on: boolean) => void;
  onGo: (step: WizardStep, problem: ConfigProblem) => void;
}) {
  const names = (h: string) => seatName(data.seats, h);
  const secretNoun = draft.method === "token" ? "token" : "key";
  const orphans = orphansOf(data, draft);
  const changes = seatChangesOf(data, draft.key, served, draft.uses);
  const stored = (what: string) =>
    keptCredential ? (
      `${what}, the stored one`
    ) : (
      <>
        {what}, stored as <span className="mono">{draft.variable}</span>
      </>
    );
  const method = {
    "api-key": stored(methodWords(draft.harness, "api-key").label),
    token: stored("Subscription token"),
    cloud: CLOUD_FIELDS[draft.cloud].label,
    "host-login": editing
      ? "Harness login on the host"
      : "Harness login on the host, made after it is saved",
    device: editing
      ? "ChatGPT sign-in with a device code"
      : "ChatGPT sign-in with a device code, right after it is saved",
    unchanged: "As Configuration sets it",
  }[draft.method];
  const id = modelId(draft);
  const asked = catalogueOf(draft.harness, id);
  const listing = useQuery("llm_models", asked ?? undefined, { enabled: !!asked });
  const named = asked ? nameIn(listing.data, id) : null;
  const home = editing ? null : defaultFleetOf(data);
  const fits = !!home && fitsFleet(home, draft);
  const tier = classOf(id) || "another class";
  const pair = home
    ? classPair(
        { class: tier, harness: draft.harness },
        { class: home.class, harness: home.harness },
      )
    : [tier, ""];

  return (
    <div className="models-stack">
      {/* WHO CHANGES MODEL FIRST, above every other summary: an edit made for a
          sign-in moved two seats, and said so only inside a summary line. */}
      {changes.length > 0 ? (
        <ChangeGroups heading="Who uses a model changes from today" groups={changes} />
      ) : null}
      <div className="models-review-head">
        <HarnessMark harness={draft.harness} size={MARK_SIZE} named tooltip />
        <span className="col models-review-head__names">
          <Text as="span" variant="body" className="mono models-clamp" title={draft.key}>
            {draft.key || "New model"}
          </Text>
          {/* THE MODEL BY NAME where the engine's catalogue names it, else its id, in mono. */}
          <Text
            as="span"
            variant="caption"
            tone="secondary"
            className={id ? (named ? "models-clamp" : "mono models-clamp") : undefined}
            title={id || undefined}
          >
            {named ?? (id || "No model chosen yet")}
          </Text>
        </span>
      </div>
      {home ? (
        <Checkbox
          framed
          label={`Add to ${home.name}, the default fleet`}
          description={
            !fits
              ? `It runs ${pair[0]}; every member of ${home.name} runs ${pair[1]}, and a fleet keeps one class.`
              : first
                ? `A company's first model joins ${home.name}, where its seats and helpers run.`
                : home.members.length === 0
                  ? `${home.name} has no members yet: this model sets the class it runs.`
                  : `It runs ${tier}, as every member of ${home.name} does. Seats on the default fleet can run on it.`
          }
          checked={fits && (first || (toDefault ?? true))}
          disabled={!fits || first}
          onCheckedChange={onToDefault}
        />
      ) : null}
      <DescriptionList
        dense
        items={[
          ["Method", method] as const,
          ...(factsOf(draft.harness).efforts.length > 0
            ? [["Reasoning effort", draft.effort || "Harness default"] as const]
            : []),
          ...(draft.harness === "qwen-code" && draft.endpoint.trim()
            ? [["Endpoint", draft.endpoint.trim()] as const]
            : []),
          ...(canExecute(draft)
            ? [
                [
                  "Where it runs",
                  // NOTHING SET UP: this save sets up this engine's host (`nextDocument`).
                  data.doc !== undefined && data.doc !== null && !hasSandbox(data.doc)
                    ? `${RUN_IN.direct}, set up by this save`
                    : draft.runIn
                      ? RUN_IN[draft.runIn]
                      : "Where the company default says",
                ] as const,
              ]
            : []),
          [
            "Limits",
            `${draft.concurrent || DEFAULT_HARNESS_MAX_CONCURRENT} processes at once, ${draft.timeout || DEFAULT_HARNESS_TIMEOUT_SECONDS} s without progress`,
          ] as const,
          ["Web tools", webToolsWords(draft.webDenied)] as const,
          ["Test call", testRow] as const,
        ]}
      />
      {dropped.length > 0 ? (
        <ul className="models-dropped">
          {dropped.map((d) => (
            <li key={`${d.seat}-${d.job}`}>
              <Text as="span" variant="body">
                {droppedWords(d, names)}
              </Text>
            </li>
          ))}
        </ul>
      ) : null}

      {problems.length > 0 ? (
        <Callout
          variant="danger"
          title={`${problems.length} ${problems.length === 1 ? "problem" : "problems"} to fix first`}
        >
          <NoticeList
            notices={problems.map((p, i) => {
              const step = stepOf(p, draft.key);
              return {
                key: `${p.path}-${i}`,
                subject: fieldOf(p, draft.key),
                title: messageOf(p),
                // A PROBLEM NO STEP CAN FIX (a program missing on the host) offers no step.
                ...(fixable(p)
                  ? {
                      action: (
                        <button type="button" className="t-link" onClick={() => onGo(step, p)}>
                          Fix in {STEP_LABELS[step]}
                        </button>
                      ),
                    }
                  : {}),
              };
            })}
          />
        </Callout>
      ) : saving ? (
        // A SAVE SAYS WHICH OF ITS WRITES IS RUNNING: the button's spinner
        // alone left a form that looked stopped for as long as the engine took.
        <Waiting key={saving}>
          {saving === "credential"
            ? `Storing the ${secretNoun} and re-activating the company with it. The engine tests it first, with one harness run.`
            : `${editing ? "Saving" : "Creating"} the model as one revision.`}
        </Waiting>
      ) : check.kind === "checking" ? (
        <Waiting key={check.what}>
          {check.what === "credential"
            ? `Testing the ${secretNoun} with one harness run. It usually answers in 5 to 20 seconds.`
            : "Checking the change with the engine."}
        </Waiting>
      ) : check.kind === "blocked" ? (
        <Callout variant="warning" title="This cannot be saved from here yet">
          {check.message}
        </Callout>
      ) : check.kind === "ready" ? (
        <>
          {/* THE CHANGE IS VALID, which is not the test call passing: the row above says that. */}
          <Callout variant="success" title="The configuration is valid">
            {editing ? "Saving" : "Creating"} it makes one revision, applied at once on every node
            with no restart.
          </Callout>
          <ConfigWarnings
            warnings={check.warnings}
            seat={(h) => {
              const s = data.seats.find((x) => x.handle === h);
              return s && { name: s.name || h, agent: s.kind !== "human" };
            }}
          />
        </>
      ) : null}

      {orphans.length > 0 ? (
        <Text as="p" variant="caption" tone="secondary" className="models-orphans">
          {orphans.map((key) => (
            <span key={key}>
              The model <span className="mono">{key}</span> will not be used by anything once this
              is saved.{" "}
              <Link href={href(["settings", "models", key], { flow: "remove" })} size="inherit">
                Remove {key}
              </Link>
            </span>
          ))}
        </Text>
      ) : null}
    </div>
  );
}
