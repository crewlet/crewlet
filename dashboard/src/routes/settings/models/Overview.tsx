/**
 * Settings › Models, the list: how the company's models stand in four
 * figures, what is wrong with them (the engine's alarms), then every model
 * with its condition, its sign-in and who runs on it.
 *
 * A member reads the same list without the sign-in, the tokens or the actions
 * (`llm_entries`' member reading names no credential and no usage), so the one
 * page answers "why is my seat waiting" for everybody.
 *
 * STATUS IS HEALTH ALONE (design QA M14). Whether a model is in use is Used
 * by's question; Status says whether it can answer, so a new model reads "Not
 * tested", never "Not used".
 */

import type { ReactNode } from "react";
import {
  Button,
  Callout,
  Card,
  EmptyState,
  RelativeTime,
  StatCard,
  StatGroup,
  Text,
  Tooltip,
  VisuallyHidden,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  CoinsGlyph,
  CpuGlyph,
  PlusGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, listed, plural } from "~/lib/format.ts";
import { CATEGORY_JOBS, CATEGORY_ORDER, SEAT_JOBS, everythingSeats } from "~/lib/modelUses.ts";
import type { Seat } from "~/lib/seats.ts";
import { Alarms } from "./Alarms.tsx";
import { HarnessMark, MARK_SIZE } from "./HarnessMark.tsx";
import type { ModelsData, ModelView } from "./data.ts";
import { JobGlyph, type Job } from "./JobGlyph.tsx";
import { EntryMenu, HealthLine, SignInLine, type OnFlow } from "./parts.tsx";
import { ASSISTANT_USE, CATEGORY_WORDS, EVERYTHING, product } from "./words.ts";

/** One use of a model in Used by: the job, its words, and the seats on it. */
interface UseLine {
  job: Job;
  label: string;
  seats: string[];
}

/**
 * A model's uses, a line each: Everything for the seats that run every job on
 * it, then each category for the seats left, then the Dashboard Assistant.
 * The engine's `used_by` says every one of them; nothing is resolved here.
 */
export function linesOf(model: ModelView, name: (handle: string) => string): UseLine[] {
  const everything = everythingSeats(model.uses);
  const lines: UseLine[] =
    everything.length > 0
      ? [{ job: "everything", label: EVERYTHING.label, seats: everything.map(name) }]
      : [];
  for (const category of CATEGORY_ORDER) {
    const seats = new Set<string>();
    for (const job of CATEGORY_JOBS[category])
      for (const s of model.uses.jobs[job]) if (!everything.includes(s)) seats.add(s);
    if (seats.size > 0)
      lines.push({
        job: category,
        label: CATEGORY_WORDS[category].label,
        seats: [...seats].map(name).sort(),
      });
  }
  if (model.uses.assistant) lines.push({ job: "assistant", label: ASSISTANT_USE.label, seats: [] });
  return lines;
}

/** Whether anything runs on a model: a seat's job or the Dashboard Assistant, for either reader (QA M13). */
const inUse = (m: ModelView) =>
  m.uses.assistant || SEAT_JOBS.some((j) => m.uses.jobs[j].length > 0);

/**
 * Who runs on a model, at ONE HEIGHT however many uses it has: the first use
 * and a "+N" that never gives way (QA M16: an ellipsis ate the count and two
 * uses read as one), the seat count under it. The whole breakdown is the
 * tooltip and the link's name, and the model's page holds it for good.
 */
function UsedBy({ model, name }: { model: ModelView; name: (handle: string) => string }) {
  const lines = linesOf(model, name);
  if (lines.length === 0) {
    return (
      <Text as="span" variant="caption" tone="tertiary">
        Not used
      </Text>
    );
  }
  const [lead, ...rest] = lines;
  const seats = new Set(SEAT_JOBS.flatMap((j) => model.uses.jobs[j])).size;
  const said = lines
    .map((l) =>
      l.job === "assistant"
        ? "Ask Crewlet"
        : `${l.label}: ${l.seats.length > 0 ? listed(l.seats) : "no seat"}`,
    )
    .join("; ");
  return (
    <Tooltip
      content={
        <span className="models-uses-breakdown">
          {lines.map((l) => (
            <span key={l.label} className="models-job">
              <JobGlyph job={l.job} size="xs" />
              <span>
                <strong>{l.label}</strong>
                {l.job === "assistant" ? `: ${ASSISTANT_USE.hint}` : `: ${listed(l.seats)}`}
              </span>
            </span>
          ))}
        </span>
      }
      placement="top"
    >
      <a
        className="models-uses"
        href={href(["settings", "models", model.key])}
        aria-label={`Used for ${said}`}
      >
        <span className="models-uses__line">
          <span className="models-uses__lead models-job">
            <JobGlyph job={lead!.job} size="xs" />
            <span className="models-uses__label">{lead!.label}</span>
          </span>
          {rest.length > 0 ? <span className="models-uses__more">+{rest.length}</span> : null}
        </span>
        {seats > 0 ? (
          <Text as="span" variant="caption" tone="tertiary">
            {plural(seats, "seat")}
          </Text>
        ) : null}
      </a>
    </Tooltip>
  );
}

/**
 * A cell's words, whole on hover (QA M15), as a link to the row's own page.
 *
 * THE ROW'S LINK LIES OVER EVERY CELL, so a tip a pointer can reach is on an
 * element raised above it, and a raised element takes the press. So the
 * raised words are a link of their own, to the page the row opens: a press, a
 * middle click and a copied address all do what the row does, and no part of
 * the row is a spot that opens nothing. Out of the tab order and hidden from
 * assistive technology, which reach the model once, by the row's link.
 */
function RowReveal({
  model,
  tip,
  children,
}: {
  model: ModelView;
  tip: ReactNode;
  children: ReactNode;
}) {
  return (
    <Tooltip content={tip} placement="top">
      <a
        className="models-row-reveal"
        href={href(["settings", "models", model.key])}
        tabIndex={-1}
        aria-hidden="true"
      >
        {children}
      </a>
    </Tooltip>
  );
}

/**
 * The model's name and the model it runs, each one line, with its CLI's mark
 * centred beside them. The mark is not raised and does nothing of its own:
 * the row's link takes the press, and the tip names the CLI.
 */
function ModelCell({ model }: { model: ModelView }) {
  return (
    <span className="models-entry-cell">
      <HarnessMark harness={model.harness} size={MARK_SIZE} named />
      <RowReveal
        model={model}
        tip={
          <span className="mono">
            {product(model.harness)} · {model.key} · {model.model}
          </span>
        }
      >
        <span className="models-pair">
          <span className="mono models-clamp">{model.key}</span>
          <Text as="span" variant="caption" tone="secondary" className="mono models-clamp">
            {model.model}
          </Text>
        </span>
      </RowReveal>
    </span>
  );
}

/** How the model signs in, its secret's name whole on hover; nothing raised where there is no name to reveal. */
function SignInCell({ model }: { model: ModelView }) {
  const variable = model.admin?.sign_in.variable ?? null;
  return variable ? (
    <RowReveal model={model} tip={<span className="mono">{variable}</span>}>
      <SignInLine model={model} />
    </RowReveal>
  ) : (
    <SignInLine model={model} />
  );
}

export function Overview({ data, onFlow }: { data: ModelsData; onFlow: OnFlow }) {
  const now = useNow();
  const { models, admin } = data;
  const names = new Map(data.seats.map((s: Seat) => [s.handle, s.name || s.handle]));
  const name = (handle: string) => names.get(handle) ?? handle;

  const ready = models.filter((m) => m.entry.health === "ready").length;
  const refused = models.filter(
    (m) => m.entry.health === "rejected" || m.entry.health === "no_sign_in",
  ).length;
  const sitting = models.filter((m) => m.entry.health === "sitting_out").length;
  const unused = models.filter((m) => !inUse(m)).length;
  const usage = models.reduce(
    (sum, m) => {
      const u = m.admin?.usage;
      return u ? { tokens: sum.tokens + u.total_tokens, calls: sum.calls + u.calls } : sum;
    },
    { tokens: 0, calls: 0 },
  );
  const add = () => onFlow("add");

  return (
    <div className="models-page">
      {/* THE PAGE'S ONE PRIMARY ACTION IN THE PAGE BAR, where Secrets puts
          its own (QA Minor 15), not in the list's card. */}
      {admin ? (
        <PageActions>
          <Button
            leadingIcon={<PlusGlyph size="sm" />}
            variant="primary"
            size="small"
            onClick={add}
          >
            Add model
          </Button>
        </PageActions>
      ) : null}

      {!admin ? (
        <Callout variant="info" title="Read-only">
          You can see which models the company runs and whether each can answer. Changing a model or
          how it signs in takes Admin access.
        </Callout>
      ) : null}

      <Alarms data={data} onFlow={onFlow} />

      {models.length > 0 ? (
        <Card padding="none">
          <StatGroup columns={admin ? 4 : 3}>
            <StatCard
              icon={<CpuGlyph size="xs" />}
              label="Models"
              value={models.length}
              sub={unused ? `${unused} not used by any seat or Ask Crewlet` : "every one in use"}
            />
            {/* A COUNT OF THE FEW, NOT "3 OF 4" (QA Minor 14): the tile names how
                many answer and, under it, exactly how many do not. */}
            <StatCard
              icon={<ActivityGlyph size="xs" />}
              label="Ready"
              value={ready}
              unit={`of ${models.length}`}
              sub={
                ready === models.length
                  ? "every model answers"
                  : `${plural(models.length - ready, "model")} not ready`
              }
            />
            <StatCard
              icon={<TriangleAlertGlyph size="xs" />}
              label="Sitting out or refused"
              value={sitting + refused}
              sub={
                sitting + refused === 0
                  ? "no limit hit, no sign-in refused"
                  : listed([
                      ...(sitting ? [`${sitting} sitting out`] : []),
                      ...(refused ? [`${refused} cannot sign in`] : []),
                    ])
              }
              subTone={refused ? "danger" : sitting ? "warning" : undefined}
            />
            {admin ? (
              <StatCard
                icon={<CoinsGlyph size="xs" />}
                label="Tokens, 7 days"
                value={fmtCount(usage.tokens)}
                sub={
                  usage.calls === 0 ? "no calls in 7 days" : `over ${plural(usage.calls, "call")}`
                }
              />
            ) : null}
          </StatGroup>
        </Card>
      ) : null}

      <Card padding="none">
        {/* NO COUNT ON THE HEAD: the Models tile above says how many (QA Minor 14). */}
        <Card.Header icon={<CpuGlyph size="sm" />}>
          <Card.Title>Models</Card.Title>
        </Card.Header>
        {models.length === 0 ? (
          <EmptyState
            size="compact"
            icon={<CpuGlyph size="xl" />}
            title="No model yet"
            description={
              admin
                ? "Add one to give the agents a model to run on."
                : "An admin adds the models the agents run on."
            }
            action={
              admin ? (
                <Button variant="primary" size="small" onClick={add}>
                  Add model
                </Button>
              ) : undefined
            }
          />
        ) : (
          <DataGrid<ModelView>
            name="models"
            rows={models}
            rowKey={(m) => m.key}
            rowHref={(m) => href(["settings", "models", m.key])}
            columns={[
              {
                key: "model",
                header: "Model",
                // A key and a model id on one line each, clamped (QA M15).
                floor: "12rem",
                sortValue: (m) => m.key,
                cell: (m) => <ModelCell model={m} />,
              },
              {
                key: "status",
                header: "Status",
                floor: "9rem",
                sortValue: (m) => m.entry.health,
                cell: (m) => <HealthLine model={m} compact />,
              },
              ...(admin
                ? [
                    {
                      key: "signin",
                      header: "Sign-in",
                      floor: "10rem",
                      drop: 2,
                      sortValue: (m: ModelView) => m.entry.sign_in.route,
                      cell: (m: ModelView) => <SignInCell model={m} />,
                    },
                  ]
                : []),
              {
                key: "uses",
                header: "Used by",
                // The first use and its "+N", whole (QA M16). Who runs on a
                // model is the list's point after its status: it gives way last.
                floor: "11rem",
                drop: 3,
                cell: (m) => <UsedBy model={m} name={name} />,
              },
              {
                key: "last",
                header: "Last answer",
                align: "right",
                shrink: true,
                // The first to give way, below a desktop width only: at 1280
                // and wider every column above fits beside it (QA Minor 17).
                drop: 1,
                sortValue: (m) => m.entry.last_success ?? "",
                cell: (m) =>
                  m.entry.last_success ? (
                    <RelativeTime value={m.entry.last_success} now={now} />
                  ) : (
                    <Text as="span" variant="caption" tone="tertiary">
                      None yet
                    </Text>
                  ),
              },
              ...(admin
                ? [
                    {
                      key: "actions",
                      // A HEAD THAT NAMES THE COLUMN to assistive technology,
                      // and draws nothing (QA Minor 17: axe's empty header).
                      header: <VisuallyHidden>Actions</VisuallyHidden>,
                      action: true,
                      shrink: true,
                      cell: (m: ModelView) => <EntryMenu model={m} onFlow={onFlow} />,
                    },
                  ]
                : []),
            ]}
          />
        )}
      </Card>
    </div>
  );
}
