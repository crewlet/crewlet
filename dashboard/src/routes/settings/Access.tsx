/**
 * Settings › People & access: the people in the chart, where agents reach
 * them, and the keys that reach the company through this engine — how far,
 * and as whom.
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
 * person with the state of their binding, and each token with its role and the
 * person it acts as.
 *
 * # Two facts per key, never folded into one
 *
 * A key's ROLE says how far it reaches — a member reads what the company
 * published, an admin also what the machine processed and how it is run
 * (ADR-0031) — and its LINK says who it acts as. A member key nobody links
 * reads and acts as nobody; an admin key nobody links is a pipeline's. So the
 * grid draws the two as two columns, and the posture above it says in words
 * what each role and a caller with no key reach.
 *
 * # Labels, never values
 *
 * The answer has no member a token's value could travel in, and this screen
 * holds none either — not even the one this browser presents, which lives in
 * the token dialog and nowhere on a page. A contact is drawn as WRITTEN: a
 * `${VAR}` is its name, never the variable's value.
 *
 * Admin-only, like every other answer on this column: which labels the
 * guard accepts, how far each reaches and whom each one is, is a map of which
 * credential to take.
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
  AnonymousAccess,
  TokenRole,
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

/**
 * The two roles, in words: what a key of each reaches (ADR-0031).
 *
 * EXHAUSTIVE OVER THE CONTRACT'S UNION, so a role the engine adds fails the
 * typecheck here rather than drawing as nothing.
 */
export const ROLE_WORDS: Record<TokenRole, { label: string; hint: string }> = {
  member: {
    label: "Member",
    hint: "Reads what the company published — its work, its pages, its chart — and acts from the dashboard as the person it is bound to. Never a transcript, spend, the configuration or a secret.",
  },
  admin: {
    label: "Admin",
    hint: "Everything a member reads, plus what the agents processed — transcripts, events, spend — and how the engine is run: /config, /secrets, /setup, nodes and backups.",
  },
};

/**
 * What a caller presenting NO key reaches, in words — the posture
 * `api.auth.anonymous` names.
 *
 * EXHAUSTIVE OVER THE CONTRACT'S UNION, for the reason [ROLE_WORDS] is.
 */
export const ANONYMOUS_WORDS: Record<AnonymousAccess, string> = {
  public:
    "A caller with no key reads the company's public face — its name, its mission and its chart — and nothing else (anonymous: public).",
  none: "A caller with no key reads nothing of the company: the health probes and this dashboard's empty shell, and no more (anonymous: none).",
};

export function PeopleAndAccess() {
  const { data, loading, error } = useQuery("access", undefined, { pollMs: POLL_MS });
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);

  const people = useMemo(() => data?.people ?? [], [data]);
  const tokens = useMemo(() => data?.tokens ?? [], [data]);
  const bound = people.filter((p) => p.binding === "bound").length;
  const broken = people.filter((p) => p.binding === "unresolved" || p.binding === "no_token");
  const adminTokens = tokens.filter((t) => t.role === "admin").length;
  const memberTokens = tokens.filter((t) => t.role === "member").length;

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
                sub={`${adminTokens} admin · ${memberTokens} member`}
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
                    : "No token is configured: nobody can act as themself or run the engine until one is added under api.auth.tokens, with its role."}
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
                    cell: (t) => <ActsAs seat={t.seat} role={t.role} />,
                  },
                  {
                    key: "role",
                    header: "Role",
                    shrink: true,
                    sortValue: (t) => t.role,
                    cell: (t) => (
                      // NEUTRAL FOR BOTH: the accent means where the reader
                      // is, and a role is a fact its word carries.
                      <Tag appearance="outline" title={ROLE_WORDS[t.role].hint}>
                        {ROLE_WORDS[t.role].label}
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
 * Where tokens are declared, and the posture beside them, in words.
 *
 * THE KEY, NOT THE TONE'S OWN INFO MARK, as Secrets' strip does: this is about
 * where a credential lives and who can hold one.
 *
 * THE POSTURE IS THREE SENTENCES, one per kind of caller: what a member key
 * reaches, what an admin key adds, and what a caller with no key reaches
 * (`api.auth.anonymous`). Each is the engine's rule stated once, so a person
 * deciding which key to hand a teammate reads it where they read the keys.
 *
 * WHO MAY CHANGE THE COMPANY is part of that posture (ADR-0030): a managed
 * document names its writers in Tier A beside the tokens, and this is the
 * screen an admin reads the deployment's `api.auth` on — so it says which
 * admin keys write the company, and that every other admin key only reads it.
 * Said only when it is managed: "every admin key may" is the default nobody
 * configured.
 */
function TierACallout({ auth }: { auth: AccessAuth }) {
  const origins = auth.allowed_origins;
  const writers = auth.company_writers;
  return (
    <Callout variant="neutral" icon={<KeyGlyph size="md" />}>
      <span className="col" style={{ gap: 4 }}>
        <span>
          API tokens are declared in Tier A under <InlineCode>api.auth.tokens</InlineCode> — the
          operator&rsquo;s bootstrap file, changed by a restart, never from here. Each has an{" "}
          <InlineCode>id</InlineCode>, recorded as the author of what it writes, a{" "}
          <InlineCode>role</InlineCode>, and a value this page never sees. A person acts as themself
          when their seat names that id in <InlineCode>contact.crewlet_operator_id</InlineCode>.
        </span>
        <span className="t-caption">
          A member key: {lowerFirst(ROLE_WORDS.member.hint)} An admin key:{" "}
          {lowerFirst(ROLE_WORDS.admin.hint)}
        </span>
        <span className="t-caption">
          {ANONYMOUS_WORDS[auth.anonymous]}{" "}
          {origins.length
            ? `Browsers may also call from ${origins.join(", ")}.`
            : "Browsers may call from this origin only."}
        </span>
        {writers.length > 0 && (
          <span className="t-caption">
            The company document is managed: only{" "}
            {writers.length === 1 ? "the admin key" : "the admin keys"}{" "}
            {writers.map((id, i) => (
              <span key={id}>
                {i > 0 && ", "}
                <InlineCode>{id}</InlineCode>
              </span>
            ))}{" "}
            may change it (<InlineCode>api.auth.company_writers</InlineCode>); every other admin key
            reads it, and can still rotate a credential it names.
          </span>
        )}
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

/**
 * The person a token acts as, or the plain fact that it acts as nobody — and
 * what that leaves it: an admin key nobody binds still changes the
 * configuration under its own label, and a member key nobody binds changes
 * nothing at all, since every change a member makes is made as a person.
 */
function ActsAs({ seat, role }: { seat: AccessSeat | null; role: TokenRole }) {
  if (!seat) {
    return (
      <EmptyValue
        label={role === "admin" ? "Nobody — writes under its own label" : "Nobody — reads only"}
      />
    );
  }
  return <SeatCell handle={seat.handle} name={seat.name} kind="human" />;
}

/** A sentence's first letter in lower case, to follow a colon. */
function lowerFirst(text: string): string {
  return text.charAt(0).toLowerCase() + text.slice(1);
}
