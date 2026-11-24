/**
 * Models & Fleets, the one page: what is wrong (the engine's alarms), four
 * figures, every model with its condition and sign-in, then the fleets and
 * who runs on which model (`SeatJobs.tsx`). With no model it is one empty
 * state ([FirstRun]): what a model is, and the add.
 *
 * A member reads the same list without the sign-in, the tokens or the actions
 * (`llm_entries`' member reading names no credential and no usage), so the one
 * page answers "why is my seat waiting" for everybody.
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
import { CpuGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount } from "~/lib/format.ts";
import { Alarms } from "./Alarms.tsx";
import { HarnessMark, MARK_SIZE } from "./HarnessMark.tsx";
import type { ModelsData, ModelView } from "./data.ts";
import { EntryMenu, HealthOneLine, type OnFlow } from "./parts.tsx";
import { WhoRuns } from "./SeatJobs.tsx";
import { UsageLimitsCell } from "./UsageLimits.tsx";
import { ROUTE, product } from "./words.ts";

/** Each condition, worst first: the order a Status sort reads in. */
const HEALTH_ORDER: readonly ModelView["entry"]["health"][] = [
  "rejected",
  "no_sign_in",
  "sitting_out",
  "expiring",
  "untested",
  "ready",
];

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
 * The model's name and the model it runs, each one line, with its harness's mark
 * centred beside them. The mark is not raised and does nothing of its own:
 * the row's link takes the press, and the tip names the harness.
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
          {/* THE MODEL BY NAME where the engine's catalogue names it, else its id, in mono. */}
          <Text
            as="span"
            variant="caption"
            tone="secondary"
            className={model.modelName === model.model ? "mono models-clamp" : "models-clamp"}
          >
            {model.modelName}
          </Text>
        </span>
      </RowReveal>
    </span>
  );
}

/** How the model signs in, its method alone: its own page names the secret it reads. */
function SignInCell({ model }: { model: ModelView }) {
  return <span>{ROUTE[model.entry.sign_in.route]}</span>;
}

export function Overview({ data, onFlow }: { data: ModelsData; onFlow: OnFlow }) {
  const now = useNow();
  const { models, admin } = data;
  const ready = models.filter((m) => m.entry.health === "ready").length;
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

      {models.length === 0 ? <FirstRun data={data} onAdd={add} /> : null}

      {models.length > 0 ? (
        <Card padding="none">
          <StatGroup columns={admin ? 4 : 3}>
            <StatCard label="Models" value={models.length} subLines={0} />
            <StatCard label="Ready" value={ready} unit={`of ${models.length}`} subLines={0} />
            {admin ? (
              <StatCard label="Tokens, 7 days" value={fmtCount(usage.tokens)} subLines={0} />
            ) : null}
            <StatCard label="Fleets" value={data.fleets?.fleets.length ?? 0} subLines={0} />
          </StatGroup>
        </Card>
      ) : null}

      {models.length > 0 ? (
        <Card as="section">
          <Card.Header
            icon={<CpuGlyph size="sm" />}
            subtitle="A model and its sign-in: a plan, an API key or a gateway."
            subtitlePlacement="below"
          >
            <Card.Title>Models</Card.Title>
          </Card.Header>
          <Card.Body padding="none">
            <DataGrid<ModelView>
              name="models"
              defaultSort="model"
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
                  shrink: true,
                  // Worst first: a model that cannot answer leads an ascending sort.
                  sortValue: (m) => HEALTH_ORDER.indexOf(m.entry.health),
                  cell: (m) => <HealthOneLine model={m} />,
                },
                {
                  key: "limits",
                  header: "Usage limits",
                  // Never gives way: on a phone it stacks into the row's card. The
                  // Fleets table's Usage limits takes the same width.
                  width: "var(--models-limits-width)",
                  cell: (m) => <UsageLimitsCell model={m} />,
                },
                ...(admin
                  ? [
                      {
                        key: "signin",
                        header: "Sign-in",
                        shrink: true,
                        drop: 2,
                        sortValue: (m: ModelView) => ROUTE[m.entry.sign_in.route],
                        cell: (m: ModelView) => <SignInCell model={m} />,
                      },
                    ]
                  : []),
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
          </Card.Body>
        </Card>
      ) : null}

      {/* UNDER THE MODELS: the fleets and who runs on which. Drawn once there is a model to pick. */}
      {models.length > 0 ? <WhoRuns data={data} /> : null}
    </div>
  );
}

/** The page with no model: what a model is, in a line, and the add. */
function FirstRun({ data, onAdd }: { data: ModelsData; onAdd: () => void }) {
  return (
    <Card padding="none">
      <EmptyState
        size="compact"
        icon={false}
        title="No model yet"
        description={`A model is a coding harness, such as Claude Code or Codex, signed in to its vendor. ${
          data.admin ? "Add one and choose who runs on it." : "An admin adds models here."
        }`}
        {...(data.admin
          ? {
              action: (
                <Button
                  leadingIcon={<PlusGlyph size="sm" />}
                  variant="primary"
                  size="small"
                  onClick={onAdd}
                >
                  Add model
                </Button>
              ),
            }
          : {})}
      />
    </Card>
  );
}
