/**
 * The model alarms, as the engine raised them (`llm_entries`' `alarms`): what
 * is wrong, who it stops, and the ONE gesture that fixes it.
 *
 * THE ENGINE DECIDES WHAT IS AN ALARM. The page never raises one from a
 * model's condition: it draws the engine's five kinds, each listing the models
 * it holds for, so the list, `/health` and the log lines cannot disagree. What
 * the page adds is the reading a person needs: one alarm per credential or
 * plan rather than per model (models sharing a plan pause together, models
 * reading one secret are fixed together), the seats it stops by their names,
 * and the jobs by theirs (design QA M8).
 *
 * THE VENDOR'S SENTENCE NEVER LEADS (QA M11). The engine's `detail` is its own
 * measurement, with the vendor's words appended ("401 invalid x-api-key"), and
 * a vendor's advice can name a command other than the one this page gives. The
 * title names the cause and the body the effect, in plain words; what the
 * engine recorded stays one disclosure away, whole.
 */

import type { ReactNode } from "react";
import { ChevronDownGlyph } from "@crewlethq/icons/glyphs";
import { Button, Callout, Disclosure, InlineCode, Menu, RelativeTime, Text } from "@crewlethq/ui";
import type { ModelAlarmKind } from "~/contract/models.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtClock, fmtDate, listed, plural, tsKey } from "~/lib/format.ts";
import { CATEGORY_JOBS, CATEGORY_ORDER, type JobCategory } from "~/lib/modelUses.ts";
import type { Seat } from "~/lib/seats.ts";
import type { ModelAlarm } from "~/protocol/models.ts";
import type { ModelsData, ModelView } from "./data.ts";
import {
  credentialAction,
  type Flow,
  type OnFlow,
  type WizardAdd,
  type WizardAt,
} from "./parts.tsx";
import { ALARM_TONE, CATEGORY_WORDS, WINDOW, product } from "./words.ts";

/** The gesture that fixes an alarm: a flow, opened on one model. */
export interface AlarmFix {
  label: string;
  flow: Flow;
  key: string;
  at?: WizardAt;
}

/**
 * A limit's second gesture: move the seats it holds to a model that can
 * answer. The uses are every seat's category on the limited models; the
 * models offered are the ones that can take all of them now.
 */
export interface AlarmMove {
  add: WizardAdd[];
  to: ModelView[];
}

/** One alarm as the page draws it: one credential or one plan, and the models on it. */
export interface AlarmCard {
  id: string;
  kind: ModelAlarmKind;
  tone: "danger" | "warning";
  keys: string[];
  title: ReactNode;
  body: ReactNode;
  /** Each model's own line as the engine recorded it, vendor's words included. */
  recorded: { key: string; detail: string }[];
  /** Who it stops, a line per job, for the disclosure. */
  stops: string[];
  fix: AlarmFix | null;
  /** For a limit holding seats, the way to move them; null otherwise. */
  move: AlarmMove | null;
}

const LIMIT_KINDS: readonly ModelAlarmKind[] = ["llm_limit", "llm_limit_waiting"];

/** A model that answers now: ready, or signing in until its credential expires. */
const ANSWERS = new Set(["ready", "expiring"]);

const CREDENTIAL_KINDS: readonly ModelAlarmKind[] = [
  "llm_sign_in_refused",
  "llm_credential_expiring",
  "llm_credential_expires_soon",
];

/**
 * Up to this many seats are named in an alarm's one line; more are counted,
 * and every name is in its disclosure. Three names fit one line of a callout
 * at a desktop width; a fourth wrapped the line it was meant to keep short.
 */
const NAMED_SEATS = 3;

const DAY_MS = 86_400_000;

/** "The model default", "The models aux and assistant": a key never opens a sentence bare (QA Nit 5). */
function theModels(keys: readonly string[], capital = true): string {
  const the = capital ? "The" : "the";
  return keys.length === 1 ? `${the} model ${keys[0]}` : `${the} models ${listed(keys)}`;
}

/** What a model signs in with, by its route, as a noun: "subscription token", "API key". */
function credentialNoun(model: ModelView | undefined): string {
  const signIn = model?.admin?.sign_in;
  if (!signIn) return "sign-in";
  if (signIn.route === "api-key" || signIn.mode === "api-key") return "API key";
  if (signIn.route === "token") return "subscription token";
  if (signIn.route === "credentials") return "CLI login";
  if (signIn.route === "env") return "cloud provider sign-in";
  return "credential";
}

/**
 * The models one alarm groups together. A credential alarm is about the
 * secret, so the models reading one variable are one alarm and one fix; a
 * limit is the plan's, so the models sharing one sign-in (`plan`) pause
 * together and are one alarm. Anything else stands alone.
 */
function groupOf(kind: ModelAlarmKind, key: string, model: ModelView | undefined): string {
  const admin = model?.admin;
  if (!admin) return `key:${key}`;
  if (CREDENTIAL_KINDS.includes(kind) && admin.sign_in.variable)
    return `var:${admin.sign_in.variable}`;
  return admin.plan ? `plan:${admin.plan}` : `key:${key}`;
}

/** A seat's display name, or its handle where the org names no such seat. */
function seatNamer(seats: readonly Seat[]): (handle: string) => string {
  const names = new Map(seats.map((s) => [s.handle, s.name || s.handle]));
  return (handle) => names.get(handle) ?? handle;
}

/** Seats in one line: named while there are few, counted past that. */
function whoLine(names: readonly string[]): string {
  return names.length <= NAMED_SEATS ? listed(names) : plural(names.length, "seat");
}

/** The seats each category runs on the models, as the engine's `used_by` says: a seat is in a category when any of its jobs there does. */
function seatsByCategory(models: readonly ModelView[]): Map<JobCategory, Set<string>> {
  const out = new Map<JobCategory, Set<string>>();
  for (const category of CATEGORY_ORDER) {
    const handles = new Set<string>();
    for (const m of models)
      for (const job of CATEGORY_JOBS[category]) for (const s of m.uses.jobs[job]) handles.add(s);
    if (handles.size > 0) out.set(category, handles);
  }
  return out;
}

/**
 * Who the models stop, a clause per category: "coding for PM and Designer",
 * "routines and evaluations for every seat", and the Dashboard Assistant.
 */
function stopsOf(models: readonly ModelView[], seats: readonly Seat[]): string[] {
  const name = seatNamer(seats);
  const out: string[] = [];
  for (const [category, handles] of seatsByCategory(models)) {
    const who =
      seats.length > 0 && handles.size >= seats.length
        ? "every seat"
        : whoLine([...handles].map(name).sort());
    out.push(`${categoryWords(category)} for ${who}`);
  }
  if (models.some((m) => m.uses.assistant)) out.push("the Dashboard Assistant");
  return out;
}

/**
 * Moving the seats a limit holds: each seat's category on the limited models,
 * and the models that can take every one of them now. Not the assistant: it
 * is company-wide, and moving it is its own choice on By job.
 *
 * NEVER BACK ONTO THE LIMIT: neither a limited model nor, unless the window is
 * the one model's own, any model on the same plan, which a session or weekly
 * limit holds too even before the engine has seen it refused. Coding goes only
 * to a CLI that runs it.
 */
function moveOf(models: readonly ModelView[], all: readonly ModelView[]): AlarmMove | null {
  const add: WizardAdd[] = [];
  for (const [job, handles] of seatsByCategory(models))
    for (const seat of [...handles].sort()) add.push({ job, seat });
  if (add.length === 0) return null;
  const held = new Set(models.map((m) => m.key));
  const own = models.some((m) => m.entry.sitting_out?.window === "model");
  const plans = new Set(own ? [] : models.flatMap((m) => (m.admin?.plan ? [m.admin.plan] : [])));
  const coding = add.some((a) => a.job === "coding");
  const to = all.filter(
    (m) =>
      !held.has(m.key) &&
      !(m.admin?.plan && plans.has(m.admin.plan)) &&
      ANSWERS.has(m.entry.health) &&
      (m.executor || !coding),
  );
  return { add, to };
}

const categoryWords = (c: JobCategory) => CATEGORY_WORDS[c].label.toLowerCase();

/** A sentence's clauses: "a; b; and c", since a clause carries its own "and". */
function clauses(parts: readonly string[]): string {
  if (parts.length < 2) return parts.join("");
  return `${parts.slice(0, -1).join("; ")}; and ${parts.at(-1) ?? ""}`;
}

/** The seats waiting on the models' limit, by name, and since when the first began. */
function waitingOf(models: readonly ModelView[], seats: readonly Seat[]) {
  const name = seatNamer(seats);
  const handles = new Set<string>();
  let since = "";
  for (const m of models) {
    const w = m.entry.waiting;
    if (!w) continue;
    w.seats.forEach((s) => handles.add(s));
    if (!since || tsKey(w.since) < tsKey(since)) since = w.since;
  }
  return { names: [...handles].map(name).sort(), since };
}

/** The fix a credential alarm offers: the model's own credential gesture, or its Method step. */
function credentialFix(model: ModelView | undefined, key: string): AlarmFix | null {
  if (!model?.admin) return null;
  const action = credentialAction(model);
  if (action)
    return { label: action.label, flow: action.flow, key, ...(action.at ? { at: action.at } : {}) };
  return { label: "Change how it signs in", flow: "edit", key, at: { step: "signin" } };
}

/**
 * Every alarm the engine raised, as the cards the page draws, worst first.
 * Pure over the reading and the instant, so the words are tested as written.
 */
export function alarmCards(data: Pick<ModelsData, "alarms" | "models" | "seats">, now: number) {
  const byKey = new Map(data.models.map((m) => [m.key, m]));
  const cards: AlarmCard[] = [];
  for (const alarm of data.alarms) {
    const groups = new Map<string, NonNullable<ModelAlarm["entries"]>>();
    for (const entry of alarm.entries ?? []) {
      const id = groupOf(alarm.kind, entry.key, byKey.get(entry.key));
      groups.set(id, [...(groups.get(id) ?? []), entry]);
    }
    // AN ALARM THAT NAMES NO MODEL is still the engine's alarm: drawn in its
    // own words, with no gesture, rather than dropped.
    if (groups.size === 0) {
      cards.push({
        id: alarm.kind,
        kind: alarm.kind,
        tone: ALARM_TONE[alarm.kind],
        keys: [],
        title: alarm.detail,
        body: null,
        recorded: [],
        stops: [],
        fix: null,
        move: null,
      });
      continue;
    }
    for (const [id, entries] of groups) {
      const keys = entries.map((e) => e.key);
      const models = keys.map((k) => byKey.get(k)).filter((m): m is ModelView => !!m);
      const stops = stopsOf(models, data.seats);
      cards.push({
        id: `${alarm.kind}:${id}`,
        kind: alarm.kind,
        tone: ALARM_TONE[alarm.kind],
        keys,
        ...words(alarm.kind, keys, models, data.seats, entries, stops, now),
        recorded: entries.map((e) => ({ key: e.key, detail: e.detail })),
        stops,
        fix: CREDENTIAL_KINDS.includes(alarm.kind)
          ? credentialFix(models[0], keys[0]!)
          : { label: "Retry now", flow: "retry", key: keys[0]! },
        move: LIMIT_KINDS.includes(alarm.kind) ? moveOf(models, data.models) : null,
      });
    }
  }
  const rank = { danger: 0, warning: 1 } as const;
  return cards.sort((a, b) => rank[a.tone] - rank[b.tone]);
}

/** An alarm's title and its one body line, by kind. */
function words(
  kind: ModelAlarmKind,
  keys: string[],
  models: ModelView[],
  seats: readonly Seat[],
  entries: NonNullable<ModelAlarm["entries"]>,
  stops: readonly string[],
  now: number,
): { title: ReactNode; body: ReactNode } {
  const first = models[0];
  const noun = credentialNoun(first);
  const variable = models.length > 1 ? first?.admin?.sign_in.variable : null;
  const shared = variable ? (
    <>
      They share <InlineCode>{variable}</InlineCode>.{" "}
    </>
  ) : null;
  const many = keys.length > 1;

  if (kind === "llm_sign_in_refused") {
    const expiresAt = first?.admin?.sign_in.expires_at ?? null;
    const expired = expiresAt !== null && tsKey(expiresAt) <= now;
    const title =
      first?.entry.health === "no_sign_in" ? (
        `${theModels(keys)} ${many ? "are" : "is"} not signed in`
      ) : expired ? (
        <>
          The {noun} for {theModels(keys, false)} expired{" "}
          <RelativeTime value={expiresAt} now={now} />
        </>
      ) : (
        `The ${noun} for ${theModels(keys, false)} was refused`
      );
    return {
      title,
      body: (
        <>
          {shared}
          {stops.length > 0
            ? `It stops ${clauses(stops)}, except where a seat has a fallback model.`
            : "No seat runs on it yet."}
        </>
      ),
    };
  }

  if (kind === "llm_credential_expiring" || kind === "llm_credential_expires_soon") {
    const at = first?.admin?.sign_in.expires_at ?? entries[0]?.since ?? null;
    const days = at ? Math.max(1, Math.ceil((tsKey(at) - now) / DAY_MS)) : null;
    const renew = noun === "API key" ? "Replace it" : "Renew it";
    return {
      title: days
        ? `The ${noun} for ${theModels(keys, false)} expires in ${plural(days, "day")}`
        : `The ${noun} for ${theModels(keys, false)} expires soon`,
      body: (
        <>
          {shared}
          {at ? `It signs in until ${fmtDate(at)}. ` : null}
          {renew} before then: the stored one keeps working until the new one passes its test.
          {stops.length > 0 ? ` Once it expires it stops ${clauses(stops)}.` : null}
        </>
      ),
    };
  }

  // A LIMIT: the plan's, or the one model's where the window is the model's.
  const out = models.find((m) => m.entry.sitting_out)?.entry.sitting_out ?? null;
  const window = out?.window ?? "unknown";
  const agent = first?.agent ?? "";
  const subject =
    window === "model"
      ? theModels(keys, false)
      : `the ${product(agent)} ${first?.admin?.sign_in.mode === "api-key" ? "API key" : "plan"}`;
  const waiting = waitingOf(models, seats);
  const pause = out ? (
    <>
      {theModels(keys)} {many ? "sit" : "sits"} out until {fmtClock(out.until, now)} (
      <RelativeTime value={out.until} now={now} />
      ).{" "}
    </>
  ) : null;
  const together =
    many && window !== "model" ? "They share one plan, so they pause together. " : null;
  const waitLine =
    waiting.names.length > 0 ? (
      <>
        {whoLine(waiting.names)} {waiting.names.length === 1 ? "is" : "are"} waiting, with no
        fallback model, since <RelativeTime value={waiting.since} now={now} />.{" "}
      </>
    ) : null;
  if (kind === "llm_limit_waiting") {
    return {
      title: `Seats are waiting for ${subject}'s ${WINDOW[window]} to reset`,
      body: (
        <>
          {waitLine}
          {pause}
          {together}
        </>
      ),
    };
  }
  return {
    title: `${capital(subject)} reached its ${WINDOW[window]}`,
    body: (
      <>
        {pause}
        {together}
        {waitLine}
        Seats with a fallback model use it meanwhile.
      </>
    ),
  };
}

const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/**
 * The menu that moves a limit's seats: each model that can take them opens
 * its wizard at Use it for with them added, and adding a model starts with
 * them added too. Review shows each seat it moves before anything is written.
 * With no model to move to, the menu says so above Add a model.
 */
function MoveMenu({ move, onFlow }: { move: AlarmMove; onFlow: OnFlow }) {
  const label = "Use a different model for these seats";
  return (
    <Menu
      label={label}
      trigger={label}
      trailingIcon={<ChevronDownGlyph size="sm" />}
      triggerVariant="secondary"
      align="end"
      items={[
        ...(move.to.length > 0
          ? move.to.map((m) => ({
              key: m.key,
              label: m.key,
              description: m.model,
              onSelect: () => onFlow("edit", m.key, { step: "assign", add: move.add }),
            }))
          : // NOTHING TO MOVE TO is said, not an empty list: the way out left is adding one.
            [
              {
                key: "none",
                label: "No other model can answer right now",
                disabled: true,
                onSelect: () => {},
              },
            ]),
        { kind: "separator" as const, key: "sep" },
        {
          key: "add",
          label: "Add a model",
          onSelect: () => onFlow("add", undefined, { step: "cli", add: move.add }),
        },
      ]}
    />
  );
}

/** The alarms, worst first, each with its one fixing gesture. An admin's alone: a member's reading carries none. */
export function Alarms({ data, onFlow }: { data: ModelsData; onFlow: OnFlow }) {
  const now = useNow();
  const cards = alarmCards(data, now);
  if (!data.admin || cards.length === 0) return null;
  return (
    <div className="models-alarms" role="region" aria-label="Model alarms">
      {cards.map((card) => (
        <Callout
          key={card.id}
          variant={card.tone}
          title={card.title}
          action={
            card.fix || card.move ? (
              <>
                {card.fix ? (
                  <Button
                    size="small"
                    variant="primary"
                    onClick={() => onFlow(card.fix!.flow, card.fix!.key, card.fix!.at)}
                  >
                    {card.fix.label}
                  </Button>
                ) : null}
                {card.move ? <MoveMenu move={card.move} onFlow={onFlow} /> : null}
              </>
            ) : undefined
          }
        >
          <div className="models-alarm">
            {card.body ? <span>{card.body}</span> : null}
            {card.recorded.length > 0 || card.stops.length > 0 ? (
              <Disclosure title="Details" size="compact" headingLevel="none">
                <div className="models-alarm__details">
                  {card.stops.length > 0 ? (
                    <ul className="models-alarm__list">
                      {card.stops.map((line) => (
                        <li key={line}>Stops {line}</li>
                      ))}
                    </ul>
                  ) : null}
                  {/* WHAT THE ENGINE RECORDED, whole: the vendor's own words are here, never in the lead. */}
                  <ul className="models-alarm__list">
                    {card.recorded.map((r) => (
                      <li key={r.key}>
                        <Text as="span" variant="caption" tone="secondary" className="mono">
                          {r.detail}
                        </Text>
                      </li>
                    ))}
                  </ul>
                </div>
              </Disclosure>
            ) : null}
          </div>
        </Callout>
      ))}
    </div>
  );
}
