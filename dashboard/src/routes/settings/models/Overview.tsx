/**
 * Settings › Models, the list: how the company's models stand in four
 * figures, what is wrong with them (the engine's alarms), then every model
 * with its condition, its sign-in and who runs on it.
 *
 * A member reads the same list without the sign-in, the spend or the actions
 * (`llm_entries`' member reading names no credential and no cost), so the one
 * page answers "why is my seat waiting" for everybody.
 *
 * STATUS IS HEALTH ALONE (design QA M14). Whether a model is in use is Used
 * by's question; Status says whether it can answer, so a new model reads "Not
 * tested", never "Not used".
 */

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
import { href, useNavigator } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtUsd, listed, plural } from "~/lib/format.ts";
import { CATEGORY_JOBS, CATEGORY_ORDER, SEAT_JOBS, everythingSeats } from "~/lib/modelUses.ts";
import type { Seat } from "~/lib/seats.ts";
import { Alarms } from "./Alarms.tsx";
import { CliMark, MARK_SIZE } from "./CliMark.tsx";
import type { ModelsData, ModelView } from "./data.ts";
import { JobGlyph, type Job } from "./JobGlyph.tsx";
import { EntryMenu, HealthLine, SignInLine, type OnFlow } from "./parts.tsx";
import { ASSISTANT_USE, CATEGORY_WORDS, EVERYTHING } from "./words.ts";

/** A spend in dollars, to the cent: the figure an invoice shows. */

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
        ? "the Dashboard Assistant"
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

/** The model's name and the model it runs, each one line, whole on hover (QA M15). */
function ModelCell({ model, onOpen }: { model: ModelView; onOpen: () => void }) {
  return (
    <span className="models-entry-cell">
      <CliMark agent={model.agent} size={MARK_SIZE} named tooltip onPress={onOpen} />
      <Tooltip
        content={
          <span className="mono">
            {model.key} · {model.model}
          </span>
        }
        placement="top"
      >
        {/* Raised over the row's link so the pointer reaches the tip; a press still opens the model. */}
        <span className="models-entry-cell__names" data-above-row="" onClick={onOpen}>
          <span className="mono models-clamp">{model.key}</span>
          <Text as="span" variant="caption" tone="secondary" className="mono models-clamp">
            {model.model}
          </Text>
        </span>
      </Tooltip>
    </span>
  );
}

/** The sign-in on one line per fact, its secret's name whole on hover (QA M15). */
function SignInCell({ model }: { model: ModelView }) {
  const variable = model.admin?.sign_in.variable ?? null;
  const line = (
    <span className="models-signin" data-above-row={variable ? "" : undefined}>
      <SignInLine model={model} />
    </span>
  );
  return variable ? (
    <Tooltip content={<span className="mono">{variable}</span>} placement="top">
      {line}
    </Tooltip>
  ) : (
    line
  );
}

export function Overview({ data, onFlow }: { data: ModelsData; onFlow: OnFlow }) {
  const nav = useNavigator();
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
      return u
        ? {
            cost: sum.cost + u.cost_usd,
            calls: sum.calls + u.calls,
            priced: sum.priced + u.priced_calls,
          }
        : sum;
    },
    { cost: 0, calls: 0, priced: 0 },
  );
  const add = () => onFlow("add");
  const open = (key: string) => nav.to(["settings", "models", key]);

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
              sub={unused ? `${unused} not used by any seat or the assistant` : "every one in use"}
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
                label="Spend, 7 days"
                value={fmtUsd(usage.cost)}
                sub={
                  usage.calls === 0
                    ? "no calls in 7 days"
                    : usage.priced === usage.calls
                      ? `${plural(usage.calls, "call")}, every one priced`
                      : `priced calls only; ${plural(usage.calls - usage.priced, "call")} on a subscription`
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
                cell: (m) => <ModelCell model={m} onOpen={() => open(m.key)} />,
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
