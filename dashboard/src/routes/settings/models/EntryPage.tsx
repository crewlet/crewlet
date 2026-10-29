/**
 * One model: its condition and what to do about it, how it signs in, who runs
 * on it, its last answer and test, what it spent this week, and its limits.
 *
 * EVERY CONDITION IS THE ENGINE'S. The header, the callout and the cards say
 * what `llm_entries` reports; a refused sign-in is read from the model's
 * health, never from a last failure older than the credential stored after it
 * (design QA M4).
 *
 * A MEMBER READS THE SAME PAGE without an action, a credential's name or a
 * vendor's sentence: their reading carries none of them.
 *
 * A HOST SIGN-IN IS THIS PAGE'S SIGN-IN CARD, not the edit wizard (design QA
 * Minor 12): nothing is edited, so every action that asks for it here moves
 * the reader to the card, which holds the command and the check.
 */

import { useCallback, useEffect, useMemo, useRef, type ReactNode, type RefObject } from "react";
import {
  Button,
  Callout,
  Card,
  DescriptionList,
  EntityChip,
  RelativeTime,
  Text,
} from "@crewlethq/ui";
import {
  ChartColumnGlyph,
  GaugeGlyph,
  KeyGlyph,
  ShieldCheckGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href, useParam } from "~/app/router.tsx";
import { usePageMenu } from "~/app/Shell.tsx";
import { DEFAULT_CLI_MAX_CONCURRENT, DEFAULT_CLI_TIMEOUT_SECONDS } from "~/contract/models.ts";
import { fmtClock, fmtCount, fmtDate, fmtExact, listed, plural, relTime } from "~/lib/format.ts";
import {
  CATEGORY_JOBS,
  CATEGORY_ORDER,
  categoryOfJob,
  type JobCategory,
  type SeatJob,
} from "~/lib/modelUses.ts";
import type { Seat } from "~/lib/seats.ts";
import type { ModelUse } from "~/protocol/models.ts";
import { CliMark, MARK_SIZE } from "./CliMark.tsx";
import type { ModelsData, ModelView } from "./data.ts";
import { failureWords, signInOf } from "./flows/TestDialog.tsx";
import { HostLogin } from "./HostLogin.tsx";
import { JobName } from "./JobGlyph.tsx";
import {
  EntryMenu,
  HealthLine,
  credentialAction,
  type Flow,
  type OnFlow,
  type WizardAt,
} from "./parts.tsx";
import { JOB_WORDS, seatName } from "./places.ts";
import { ROUTE, RUN_IN, WINDOW, product } from "./words.ts";

type Open = (flow: Flow, at?: WizardAt) => void;

// ---------------------------------------------------------------------------
// Who runs on it
// ---------------------------------------------------------------------------

/** A seat on a category: the jobs of it the seat holds, all of them or some. */
export interface SeatOnCategory {
  seat: string;
  jobs: SeatJob[];
  /** Whether it holds every job the category holds. */
  whole: boolean;
}

/** One category's seats on the model: those that run it, and those that fall back to it. */
export interface CategoryUse {
  category: JobCategory;
  runs: SeatOnCategory[];
  backups: SeatOnCategory[];
}

/** Whether the Dashboard Assistant runs on the model, falls back to it, or neither. */
export type AssistantUse = "runs" | "backup" | null;

export interface WhoRuns {
  categories: CategoryUse[];
  assistant: AssistantUse;
  deepWork: AssistantUse;
  /** Every distinct seat on the model, at any position. */
  seats: number;
}

/**
 * The engine's `used_by` grouped by category, each seat once per category,
 * runs (position 0) apart from backups (a later position). A seat that runs
 * one job of a category and backs up the other is said as both.
 */
export function whoRuns(usedBy: readonly ModelUse[]): WhoRuns {
  const at = (pos: "runs" | "backups") => {
    const out = new Map<JobCategory, Map<string, SeatJob[]>>();
    for (const use of usedBy) {
      if (!use.seat || !use.job) continue;
      if ((pos === "runs") !== (use.position === 0)) continue;
      const category = categoryOfJob(use.job);
      if (!category) continue;
      const seats = out.get(category) ?? new Map<string, SeatJob[]>();
      const jobs = seats.get(use.seat) ?? [];
      if (!jobs.includes(use.job as SeatJob)) jobs.push(use.job as SeatJob);
      seats.set(use.seat, jobs);
      out.set(category, seats);
    }
    return out;
  };
  const runs = at("runs");
  const backups = at("backups");
  const list = (category: JobCategory, from: Map<JobCategory, Map<string, SeatJob[]>>) =>
    [...(from.get(category) ?? new Map<string, SeatJob[]>())]
      .map(([seat, jobs]) => ({
        seat,
        jobs: CATEGORY_JOBS[category].filter((j) => jobs.includes(j)),
        whole: CATEGORY_JOBS[category].every((j) => jobs.includes(j)),
      }))
      .sort((a, b) => a.seat.localeCompare(b.seat));
  const assistantOf = (which: "chat" | "deep_work"): AssistantUse => {
    const uses = usedBy.filter((u) => u.assistant === which);
    if (uses.some((u) => u.position === 0)) return "runs";
    return uses.length > 0 ? "backup" : null;
  };
  return {
    categories: CATEGORY_ORDER.map((category) => ({
      category,
      runs: list(category, runs),
      backups: list(category, backups),
    })).filter((c) => c.runs.length > 0 || c.backups.length > 0),
    assistant: assistantOf("chat"),
    deepWork: assistantOf("deep_work"),
    seats: new Set(usedBy.flatMap((u) => (u.seat ? [u.seat] : []))).size,
  };
}

/** A seat by its name, linked to its page; the whole name on hover where it is cut short. */
function SeatChip({ seats, on }: { seats: readonly Seat[]; on: SeatOnCategory }) {
  const seat = seats.find((s) => s.handle === on.seat);
  const name = seatName(seats, on.seat);
  return (
    <li className="models-who__seat">
      <EntityChip
        name={name}
        title={name}
        kind={seat?.kind === "human" ? "human" : "agent"}
        size="xs"
        href={href(["company", "org", "seats", on.seat])}
      />
      {on.whole ? null : (
        <Text as="span" variant="body" tone="secondary">
          ({on.jobs.map((j) => JOB_WORDS[j]).join(" and ")} only)
        </Text>
      )}
    </li>
  );
}

function assistantLine(use: AssistantUse, deepWork: AssistantUse): string[] {
  return [
    ...(use === "runs" ? ["The Dashboard Assistant runs on it."] : []),
    ...(use === "backup" ? ["The Dashboard Assistant falls back to it."] : []),
    ...(deepWork === "runs" ? ["The Dashboard Assistant's deep work runs on it."] : []),
    ...(deepWork === "backup" ? ["The Dashboard Assistant's deep work falls back to it."] : []),
  ];
}

function WhoRunsCard({ model, data }: { model: ModelView; data: ModelsData }) {
  const who = useMemo(() => whoRuns(model.entry.used_by), [model.entry.used_by]);
  const assistant = assistantLine(who.assistant, who.deepWork);
  return (
    <Card padding="none">
      <Card.Header icon={<UsersGlyph size="sm" />} count={who.seats}>
        <Card.Title>Who runs on it</Card.Title>
      </Card.Header>
      <Card.Body>
        {who.categories.length === 0 ? (
          // ONE EMPTY STATE, whether or not the assistant runs here (QA M12).
          <div className="models-stack">
            <Text as="p" variant="body" tone="secondary">
              No seat uses <span className="mono">{model.key}</span>.
            </Text>
            {assistant.map((line) => (
              <Text key={line} as="p" variant="body">
                {line}
              </Text>
            ))}
          </div>
        ) : (
          <ul className="models-who" aria-label={`Who runs on ${model.key}`}>
            {who.categories.map((c) => (
              <li key={c.category} className="models-who__category">
                <JobName job={c.category} />
                <div className="models-who__seats-col">
                  {c.runs.length > 0 ? (
                    <ul className="models-who__seat-list" aria-label="Runs on it">
                      {c.runs.map((on) => (
                        <SeatChip key={on.seat} seats={data.seats} on={on} />
                      ))}
                    </ul>
                  ) : null}
                  {c.backups.length > 0 ? (
                    <Text as="p" variant="body" tone="secondary">
                      Backup for{" "}
                      {listed(
                        c.backups.map(
                          (on) =>
                            seatName(data.seats, on.seat) +
                            (on.whole
                              ? ""
                              : ` (${on.jobs.map((j) => JOB_WORDS[j]).join(" and ")})`),
                        ),
                      )}
                      .
                    </Text>
                  ) : null}
                </div>
              </li>
            ))}
            {assistant.length > 0 ? (
              <li className="models-who__category">
                <JobName job="assistant" />
                <div className="models-who__seats-col">
                  {assistant.map((line) => (
                    <Text key={line} as="p" variant="body">
                      {line}
                    </Text>
                  ))}
                </div>
              </li>
            ) : null}
          </ul>
        )}
      </Card.Body>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// The condition
// ---------------------------------------------------------------------------

/** The models sharing this one's plan that sit out with it, admin only. */
function sharingPlan(model: ModelView, data: ModelsData): string[] {
  const plan = model.admin?.plan ?? "";
  if (!plan) return [];
  return data.models
    .filter((m) => m.key !== model.key && m.admin?.plan === plan && m.entry.sitting_out)
    .map((m) => m.key);
}

/** Whether the last failure is news: never once a credential was stored after it (QA M4). */
function failureStands(model: ModelView): boolean {
  const failure = model.admin?.last_failure;
  if (!failure) return false;
  const stored = model.admin?.sign_in.stored_at;
  return !stored || Date.parse(failure.at) > Date.parse(stored);
}

function VendorSaid({ said }: { said: string | undefined }) {
  if (!said) return null;
  return (
    <Text as="span" variant="caption" tone="secondary" mono className="models-said">
      Vendor said: {said}
    </Text>
  );
}

function CredentialButton({
  model,
  open,
  variant = "primary",
}: {
  model: ModelView;
  open: Open;
  variant?: "primary" | "secondary";
}) {
  const action = credentialAction(model);
  if (!action) return null;
  return (
    <Button size="small" variant={variant} onClick={() => open(action.flow, action.at)}>
      {action.label}
    </Button>
  );
}

function ConditionCallout({
  model,
  data,
  open,
}: {
  model: ModelView;
  data: ModelsData;
  open: Open;
}) {
  const entry = model.entry;
  const admin = data.admin;
  const bench = entry.sitting_out;
  if (entry.health === "sitting_out" && bench) {
    const sharing = admin ? sharingPlan(model, data) : [];
    const waiting = entry.waiting?.seats ?? [];
    return (
      <Callout
        variant="warning"
        title={`${product(model.agent)} ${WINDOW[bench.window]}`}
        action={
          admin ? (
            <Button size="small" variant="primary" onClick={() => open("retry")}>
              Retry now…
            </Button>
          ) : undefined
        }
      >
        <span className="models-stack">
          <span>
            Calls pause until {fmtClock(bench.until, Date.now())} (
            <RelativeTime value={bench.until} />
            ).
            {sharing.length > 0
              ? ` The models ${listed(sharing)} share this plan and sit out too.`
              : ""}
            {waiting.length > 0
              ? ` ${plural(waiting.length, "seat")} ${waiting.length === 1 ? "waits" : "wait"} for it: ${listed(waiting.map((s) => seatName(data.seats, s)))}.`
              : ""}
          </span>
          {admin ? <VendorSaid said={bench.said} /> : null}
        </span>
      </Callout>
    );
  }
  if (entry.health === "rejected" || entry.health === "no_sign_in") {
    const failure = failureStands(model) ? model.admin?.last_failure : null;
    const hint = model.admin?.sign_in.hint;
    return (
      <Callout
        variant="danger"
        title={
          entry.health === "rejected" ? failureWords("auth", signInOf(model)) : "Not signed in"
        }
        action={admin ? <CredentialButton model={model} open={open} /> : undefined}
      >
        {failure ? (
          <span className="models-stack">
            <span>
              Refused <RelativeTime value={failure.at} /> on {failure.node}. Seats move to their
              backups until it signs in again.
            </span>
            <VendorSaid said={failure.said} />
          </span>
        ) : entry.health === "rejected" ? (
          "Seats move to their backups until it signs in again."
        ) : (
          (hint ?? "Nothing signs this model in yet.")
        )}
      </Callout>
    );
  }
  if (entry.health === "expiring" && model.admin?.sign_in.expires_at) {
    return (
      <Callout
        variant="warning"
        title={`The subscription token expires on ${fmtDate(model.admin.sign_in.expires_at)}`}
        action={<CredentialButton model={model} open={open} />}
      >
        It stops working then, and every seat on it moves to its backups. Renew it before then.
      </Callout>
    );
  }
  const device = model.admin?.device_sign_in;
  if (device && (device.status === "pending" || device.status === "starting")) {
    return (
      <Callout
        variant="info"
        title="A device sign-in is waiting for approval"
        action={
          <Button size="small" variant="primary" onClick={() => open("device")}>
            Show the code
          </Button>
        }
      >
        It expires <RelativeTime value={device.expires_at} />.
      </Callout>
    );
  }
  return null;
}

// ---------------------------------------------------------------------------
// The cards an admin reads
// ---------------------------------------------------------------------------

/** "Aug 1, 2026 (88 days ago)": the age is the only cue an old key gives (QA Minor 21). */
function dated(at: string): string {
  return `${fmtDate(at)} (${relTime(at, Date.now())})`;
}

function SignInCard({
  model,
  data,
  open,
  focusRef,
}: {
  model: ModelView;
  data: ModelsData;
  open: Open;
  focusRef: RefObject<HTMLElement | null>;
}) {
  const signIn = model.admin!.sign_in;
  const host = signIn.route === "credentials";
  const items: (readonly [ReactNode, ReactNode])[] = [["Method", ROUTE[signIn.route]]];
  if (signIn.variable)
    items.push([
      "Secret",
      <span className="mono models-clip" title={signIn.variable}>
        {signIn.variable}
      </span>,
    ]);
  if (signIn.variable && !signIn.resolves) items.push(["Value", "Not set on this node"]);
  else if (signIn.hint && signIn.source !== "none")
    items.push(["Value", <span className="mono">{signIn.hint}</span>]);
  if (signIn.stored_at)
    items.push([
      "Stored",
      `${dated(signIn.stored_at)}${signIn.stored_by ? ` by ${signIn.stored_by}` : ""}`,
    ]);
  else if (signIn.source === "environment") items.push(["Stored", "In the engine's environment"]);
  if (signIn.expires_at)
    items.push([
      "Expires",
      Date.parse(signIn.expires_at) <= Date.now() ? (
        <>
          Expired <RelativeTime value={signIn.expires_at} />
        </>
      ) : (
        dated(signIn.expires_at)
      ),
    ]);
  return (
    <section
      id="models-sign-in"
      ref={focusRef}
      tabIndex={-1}
      className="models-focus-target"
      aria-label="Sign-in"
    >
      <Card padding="none">
        <Card.Header
          icon={<KeyGlyph size="sm" />}
          actions={
            host ? undefined : <CredentialButton model={model} open={open} variant="secondary" />
          }
        >
          <Card.Title>Sign-in</Card.Title>
        </Card.Header>
        <Card.Body>
          <div className="models-stack">
            <DescriptionList dense items={items} />
            {host ? (
              <HostLogin modelKey={model.key} logins={signIn.host_logins} onChecked={data.reload} />
            ) : null}
            {signIn.route === "token" && model.agent === "claude-code" ? (
              <Text as="p" variant="caption" tone="tertiary">
                A token made with <span className="mono">claude setup-token</span> on your own
                computer, for your own subscription. The value is never shown again.
              </Text>
            ) : null}
          </div>
        </Card.Body>
      </Card>
    </section>
  );
}

function TestCard({ model, open }: { model: ModelView; open: Open }) {
  const admin = model.admin!;
  const failure = failureStands(model) ? admin.last_failure : null;
  return (
    <Card padding="none">
      <Card.Header
        icon={<ShieldCheckGlyph size="sm" />}
        actions={
          <Button size="small" variant="secondary" onClick={() => open("test")}>
            Test now
          </Button>
        }
      >
        <Card.Title>Answers</Card.Title>
      </Card.Header>
      <Card.Body>
        <div className="models-stack">
          <DescriptionList
            dense
            items={[
              [
                "Last answered",
                admin.last_success ? <RelativeTime value={admin.last_success} /> : "Not yet",
              ],
              ...(failure
                ? ([
                    [
                      "Last failure",
                      <span className="col">
                        <span>
                          {failureWords(failure.kind, signInOf(model))},{" "}
                          <RelativeTime value={failure.at} /> on {failure.node}
                        </span>
                        <VendorSaid said={failure.said} />
                      </span>,
                    ],
                  ] as const)
                : []),
            ]}
          />
          <Text as="p" variant="caption" tone="tertiary">
            A test runs the CLI once with a real call and costs a few tokens.
          </Text>
        </div>
      </Card.Body>
    </Card>
  );
}

function UsageCard({ model }: { model: ModelView }) {
  const usage = model.admin!.usage;
  return (
    <Card padding="none">
      <Card.Header icon={<ChartColumnGlyph size="sm" />}>
        <Card.Title>Usage, last {plural(usage.days, "day")}</Card.Title>
      </Card.Header>
      <Card.Body>
        {usage.calls === 0 ? (
          <Text as="p" variant="body" tone="secondary">
            No calls in the last {plural(usage.days, "day")}.
          </Text>
        ) : (
          <DescriptionList
            dense
            items={[
              ["Calls", fmtExact(usage.calls)],
              ["Tokens", fmtCount(usage.total_tokens)],
            ]}
          />
        )}
      </Card.Body>
    </Card>
  );
}

function LimitsCard({ model, open }: { model: ModelView; open: Open }) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<GaugeGlyph size="sm" />}
        actions={
          <Button size="small" variant="secondary" onClick={() => open("edit", { step: "limits" })}>
            Change
          </Button>
        }
      >
        <Card.Title>Limits</Card.Title>
      </Card.Header>
      <Card.Body>
        <DescriptionList
          dense
          items={[
            [
              "Timeout",
              model.timeoutSeconds === null
                ? `${DEFAULT_CLI_TIMEOUT_SECONDS} s (the default)`
                : `${model.timeoutSeconds} s`,
            ],
            [
              "Processes at once",
              model.maxConcurrent === null
                ? `${DEFAULT_CLI_MAX_CONCURRENT} (the default)`
                : String(model.maxConcurrent),
            ],
          ]}
        />
      </Card.Body>
    </Card>
  );
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

export function EntryPage({
  model,
  data,
  onFlow,
}: {
  model: ModelView;
  data: ModelsData;
  onFlow: OnFlow;
}) {
  const signInRef = useRef<HTMLElement | null>(null);
  const key = model.key;
  // A HOST SIGN-IN ASKED FOR ELSEWHERE (the list, By job, an alarm) arrives
  // here as `?focus=host-login`: the sign-in card is where it is made, and
  // the parameter is spent once it has moved focus, so Back does not move it
  // again.
  const [arrival, setArrival] = useParam("focus", "");
  useEffect(() => {
    if (arrival !== "host-login" || !signInRef.current) return;
    signInRef.current.scrollIntoView({ block: "start" });
    signInRef.current.focus();
    setArrival("");
  }, [arrival, setArrival]);
  const open: Open = useCallback(
    (flow, at) => {
      if (flow === "edit" && at?.focus === "host-login" && signInRef.current) {
        signInRef.current.scrollIntoView({ block: "start" });
        signInRef.current.focus();
        return;
      }
      onFlow(flow, key, at);
    },
    [onFlow, key],
  );
  usePageMenu(
    data.admin
      ? [
          { key: "test", label: `Test ${key}`, onSelect: () => open("test") },
          { key: "edit", label: `Edit ${key}`, onSelect: () => open("edit", { step: "cli" }) },
          { key: "remove", label: `Remove ${key}`, onSelect: () => open("remove") },
        ]
      : [],
  );

  // FOUR FACTS AT MOST, so the line never wraps at a desktop width (QA Minor
  // 2); the limits are a card of their own. Where it runs and the web tools
  // are read off the document, which only an admin reads.
  const facts: Fact[] = [
    { label: "Model", value: <span className="mono">{model.model}</span>, token: true },
    { label: "Status", value: <HealthLine model={model} compact /> },
    ...(data.admin && model.executor
      ? [
          {
            label: "Where it runs",
            value: model.runIn
              ? (RUN_IN[model.runIn as keyof typeof RUN_IN] ?? model.runIn)
              : "The company default",
          },
        ]
      : []),
    ...(data.admin ? [{ label: "Web tools", value: model.webDenied ? "Denied" : "Allowed" }] : []),
  ];

  return (
    <div className="models-page">
      {data.admin ? (
        <PageActions>
          <span className="page-action-folds">
            <EntryMenu model={model} onFlow={(f, _key, at) => open(f, at)} />
          </span>
        </PageActions>
      ) : null}
      <ObjectHeader
        kind={`${product(model.agent)} model`}
        icon="cpu"
        mark={<CliMark agent={model.agent} size={MARK_SIZE} />}
        title={key}
        facts={facts}
      />
      <ConditionCallout model={model} data={data} open={open} />
      {model.admin ? (
        <div className="models-entry-grid">
          <SignInCard model={model} data={data} open={open} focusRef={signInRef} />
          <TestCard model={model} open={open} />
        </div>
      ) : null}
      <WhoRunsCard model={model} data={data} />
      {model.admin ? (
        <div className="models-entry-grid">
          <UsageCard model={model} />
          <LimitsCard model={model} open={open} />
        </div>
      ) : null}
    </div>
  );
}
