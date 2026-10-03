/**
 * Settings › People & access: the people in the chart, where agents reach
 * them, and the credentials that reach the company through this engine — and
 * as whom.
 *
 * # One join, drawn from both ends
 *
 * A person acts from the dashboard as themself when their seat names an API
 * token's label in `contact.crewlet_operator_id` (ADR-0024). The two halves of
 * that live in two tiers — the token in Tier A, the binding in the company
 * document — and each half alone hides the half that goes wrong: a token
 * nobody binds looks like a pipeline's credential, and a binding with a typo
 * in it looks bound until that person presses a button and is refused. So the
 * engine answers the join (`access`) and this screen draws both lists: each
 * person with the state of their binding, and each token with the person it
 * acts as.
 *
 * # Labels, never values
 *
 * The answer has no member a token's value could travel in, and this screen
 * holds none either — not even the one this browser presents, which lives in
 * the token dialog and nowhere on a page. A contact is drawn as WRITTEN: a
 * `${VAR}` is its name, never the variable's value.
 *
 * Operator-only, like every other answer on this column: which labels the
 * guard accepts and whom each one is, is a map of which credential to take.
 */

import { useMemo } from "react";
import {
  Callout,
  Card,
  EmptyValue,
  InlineCode,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import { KeyGlyph, TriangleAlertGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, SeatCell, TextCell } from "~/app/frame/cells.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { href } from "~/app/router.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type {
  AccessAuth,
  AccessBinding,
  AccessContact,
  AccessPerson,
  AccessSeat,
  AccessToken,
  TokenScope,
} from "~/contract/access.ts";

/**
 * How often the answer is asked again, in ms.
 *
 * Both halves change only on an operator's hand: Tier A at a restart, the
 * binding at a config activation. A minute is soon enough to see an
 * activation made in another tab land, and the page is read, not watched.
 */
const POLL_MS = 60_000;

/**
 * The surface each contact field reaches a person on, in the chart's own
 * field order. A key this build has no word for is drawn as the key itself —
 * the engine adds a surface additively, and naming it beats dropping it.
 */
const CONTACT_SURFACES: Record<string, string> = {
  slack_user_id: "Slack",
  mattermost_user_id: "Mattermost",
  atlassian_account_id: "Atlassian",
  github_login: "GitHub",
  gitlab_username: "GitLab",
};

/**
 * What each binding state means to the person reading it, and what to do.
 *
 * EXHAUSTIVE OVER THE CONTRACT'S UNION, so a state the engine adds fails the
 * typecheck here rather than drawing as nothing.
 */
export const BINDING_WORDS: Record<
  AccessBinding,
  { label: string; tone: "success" | "neutral" | "warning"; hint: string }
> = {
  bound: {
    label: "Acts as themself",
    tone: "success",
    hint: "Their token is accepted and bound to this seat: what they change from the dashboard is recorded under their name.",
  },
  unbound: {
    label: "Not bound",
    tone: "neutral",
    hint: "The seat names no API token. Agents still reach them on the surfaces listed; they do not act through this engine as themself.",
  },
  unresolved: {
    label: "Variable unset",
    tone: "warning",
    hint: "contact.crewlet_operator_id is a ${VAR} this engine's environment does not set, so it binds no token. Set the variable, or write the label.",
  },
  no_token: {
    label: "No such token",
    tone: "warning",
    hint: "contact.crewlet_operator_id names a label api.auth.tokens does not carry, so every press they make is refused. Correct the label on either side.",
  },
};

/** The two scopes, in words. */
export const SCOPE_WORDS: Record<TokenScope, { label: string; hint: string }> = {
  person: {
    label: "Person",
    hint: "Every guarded read and write, and acting from the dashboard as the person it is bound to.",
  },
  operator: {
    label: "Operator",
    hint: "Every guarded read and write, /config, /secrets and the operator MCP surface, under its own label. It cannot act from the dashboard: that needs a token bound to a person.",
  },
};

export function PeopleAndAccess() {
  const { data, loading, error } = useQuery("access", undefined, { pollMs: POLL_MS });
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  const people = useMemo(() => data?.people ?? [], [data]);
  const tokens = useMemo(() => data?.tokens ?? [], [data]);
  const bound = people.filter((p) => p.binding === "bound").length;
  const broken = people.filter((p) => p.binding === "unresolved" || p.binding === "no_token");
  const personTokens = tokens.filter((t) => t.scope === "person").length;

  return (
    <>
      <PageActions>
        {/* THE CHART IS WHERE A PERSON AND THEIR BINDING ARE WRITTEN, so the
            one action here leaves for it rather than editing a copy. */}
        <a className="t-link" href={href(["agents", "edit"])}>
          Edit people in org →
        </a>
      </PageActions>
      <PageNote>
        The people in the chart, where agents reach them, and the API tokens that reach the company
        through this engine. Labels and bindings — this screen never holds a token&rsquo;s value.
      </PageNote>

      {error && !data && <QueryState error={error} loading={false} />}

      {data && (
        <>
          <TierACallout auth={data.auth} />
          {data.auth.disabled && (
            <Callout variant="danger" role="alert">
              <InlineCode>api.auth.disabled</InlineCode> is on: every caller is{" "}
              <InlineCode>anonymous</InlineCode>, every route serves without a token, and nobody can
              act from the dashboard as a person. It is a local-development switch.
            </Callout>
          )}

          <Card padding="none">
            <StatGroup columns={3}>
              <StatCard
                icon={<UsersGlyph size="xs" />}
                label="People"
                value={people.length}
                sub={`${bound} act${bound === 1 ? "s" : ""} here as themselves`}
              />
              <StatCard
                icon={<KeyGlyph size="xs" />}
                label="API tokens"
                value={tokens.length}
                sub={`${personTokens} bound to a person`}
              />
              <StatCard
                icon={<TriangleAlertGlyph size="xs" />}
                label="Broken bindings"
                value={broken.length}
                sub={
                  broken.length
                    ? "a seat names a token that binds nobody"
                    : "every binding reaches a token"
                }
              />
            </StatGroup>
          </Card>

          <Card padding="none">
            <Card.Header icon={<UsersGlyph size="sm" />} count={people.length}>
              People
            </Card.Header>
            {people.length === 0 ? (
              <div style={{ padding: "var(--spacing-4)" }}>
                <p className="t-body muted">
                  The chart has no human seats. Add one with <InlineCode>kind: human</InlineCode>{" "}
                  and a contact in the org builder.
                </p>
              </div>
            ) : (
              <DataGrid<AccessPerson>
                rows={people}
                rowKey={(p) => p.handle}
                defaultSort="person"
                columns={[
                  {
                    key: "person",
                    header: "Person",
                    floor: "12rem",
                    sortValue: (p) => p.name || p.handle,
                    cell: (p) => <SeatCell handle={p.handle} name={p.name} kind="human" />,
                  },
                  {
                    key: "unit",
                    header: "Team",
                    shrink: true,
                    drop: 2,
                    sortValue: (p) => index.byHandle.get(p.handle)?.unit?.name ?? "",
                    cell: (p) => {
                      const unit = index.byHandle.get(p.handle)?.unit?.name;
                      return unit ? <TextCell>{unit}</TextCell> : <EmptyValue label="No team" />;
                    },
                  },
                  {
                    key: "reach",
                    header: "Reached on",
                    cell: (p) => <Contacts contacts={p.contacts} />,
                  },
                  {
                    key: "binding",
                    header: "Acts here",
                    shrink: true,
                    sortValue: (p) => p.binding,
                    cell: (p) => <Binding binding={p.binding} />,
                  },
                  {
                    key: "token",
                    header: "Token",
                    shrink: true,
                    drop: 1,
                    sortValue: (p) => p.operator_id,
                    // THE BINDING AS WRITTEN — a label or a `${VAR}`, never a
                    // variable's value — so a typo is readable beside the
                    // state it caused.
                    cell: (p) =>
                      p.operator_id ? (
                        <KeyCell value={p.operator_id} />
                      ) : (
                        <EmptyValue label="Names no token" />
                      ),
                  },
                ]}
              />
            )}
          </Card>

          <Card padding="none">
            <Card.Header icon={<KeyGlyph size="sm" />} count={tokens.length}>
              API tokens
            </Card.Header>
            {tokens.length === 0 ? (
              <div style={{ padding: "var(--spacing-4)" }}>
                <p className="t-body muted">
                  {data.auth.disabled
                    ? "The guard is disabled, so it accepts no token at all."
                    : "No token is configured: every write, and all of /config and /secrets, is refused until one is added under api.auth.tokens."}
                </p>
              </div>
            ) : (
              <DataGrid<AccessToken>
                rows={tokens}
                rowKey={(t) => t.id}
                defaultSort="id"
                columns={[
                  {
                    key: "id",
                    header: "Label",
                    shrink: true,
                    sortValue: (t) => t.id,
                    cell: (t) => (
                      <span className="row gap-2">
                        <KeyCell value={t.id} />
                        {t.yours && <Tag variant="brand">yours</Tag>}
                      </span>
                    ),
                  },
                  {
                    key: "seat",
                    header: "Acts as",
                    floor: "10rem",
                    sortValue: (t) => t.seat?.name ?? "",
                    cell: (t) => <ActsAs seat={t.seat} />,
                  },
                  {
                    key: "scope",
                    header: "Scope",
                    shrink: true,
                    sortValue: (t) => t.scope,
                    cell: (t) => (
                      <Tag appearance="outline" title={SCOPE_WORDS[t.scope].hint}>
                        {SCOPE_WORDS[t.scope].label}
                      </Tag>
                    ),
                  },
                ]}
              />
            )}
          </Card>
        </>
      )}

      {loading && !data && <Skeleton variant="text" rows={6} label="Loading" />}
    </>
  );
}

/**
 * Where tokens are declared, and the read posture beside them.
 *
 * THE KEY, NOT THE TONE'S OWN INFO MARK, as Secrets' strip does: this is about
 * where a credential lives and who can hold one.
 */
function TierACallout({ auth }: { auth: AccessAuth }) {
  const origins = auth.allowed_origins;
  return (
    <Callout variant="neutral" icon={<KeyGlyph size="md" />}>
      <span className="col" style={{ gap: 4 }}>
        <span>
          API tokens are declared in Tier A under <InlineCode>api.auth.tokens</InlineCode> — the
          operator&rsquo;s bootstrap file, changed by a restart, never from here. Each has an{" "}
          <InlineCode>id</InlineCode>, recorded as the author of what it writes, and a value this
          page never sees. A person acts as themself when their seat names that id in{" "}
          <InlineCode>contact.crewlet_operator_id</InlineCode>.
        </span>
        <span className="t-caption">
          {auth.anonymous_read
            ? "Reads outside /config, /secrets and /setup are open without a token (allow_anonymous_read)."
            : "Every read needs a token (allow_anonymous_read is off)."}{" "}
          {origins.length
            ? `Browsers may also call from ${origins.join(", ")}.`
            : "Browsers may call from this origin only."}
        </span>
      </span>
    </Callout>
  );
}

/** Where agents reach a person: one chip per field, as written. */
function Contacts({ contacts }: { contacts: AccessContact[] }) {
  if (contacts.length === 0) return <EmptyValue label="No surface" />;
  return (
    <span className="row gap-1" style={{ flexWrap: "wrap" }}>
      {contacts.map((c) => {
        const surface = CONTACT_SURFACES[c.key] ?? c.key;
        return (
          <Tag
            key={c.key}
            size="sm"
            appearance="outline"
            variant={c.resolves ? "neutral" : "warning"}
            title={
              c.resolves
                ? `${c.key}: ${c.value}`
                : `${c.key}: ${c.value} is not set in the engine's environment, so agents cannot reach them here`
            }
          >
            {surface} <span className="mono">{c.value}</span>
          </Tag>
        );
      })}
    </span>
  );
}

/** Whether a person acts through this engine as themself, with why in its title. */
function Binding({ binding }: { binding: AccessBinding }) {
  const words = BINDING_WORDS[binding];
  return (
    <Tag size="sm" variant={words.tone} title={words.hint}>
      {words.label}
    </Tag>
  );
}

/** The person a token acts as, or the plain fact that it acts as nobody. */
function ActsAs({ seat }: { seat: AccessSeat | null }) {
  if (!seat) return <EmptyValue label="Nobody — writes under its own label" />;
  return <SeatCell handle={seat.handle} name={seat.name} kind="human" />;
}
