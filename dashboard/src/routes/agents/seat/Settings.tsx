/**
 * How a seat is configured, from the org chart's own row for it — its RUNTIME
 * half, which the chart serves only to a reader holding `config:read` and the
 * org projection deliberately does not carry.
 *
 * # Read from the chart, by handle, never from the company document
 *
 * The company document holds no seats any more: the org chart left it for a
 * log of its own, so a seat found in it by NAME was nothing for every seat in
 * every company. The profile reads the chart once ([useSeatSetup]) and hands
 * the reading here; every outcome of it — still out, refused naming the
 * grants, a node that could not answer, absent, or the runtime half withheld
 * — has its own sentence ([SettingsState]) rather than an empty value.
 *
 * # Read-only here, and it says where the change is made
 *
 * Every value on this tab is one the org editor writes, so the tab offers
 * "Edit in org" (`#/agents/edit?seat=`) rather than a second, narrower form
 * that would have its own idea of which fields exist.
 *
 * # A credential is named, never shown
 *
 * A seat's tool credentials are listed by SERVER and VARIABLE NAME, with a
 * word for what is set — a secret reference, a literal the engine masked, or
 * nothing. No value the engine sent for one reaches the page: not the
 * reference, not the mask, and not a literal a redaction bug let through.
 * What a reader needs from this tab is WHICH credentials a seat's tools are
 * given; the secret store is where their values live.
 *
 * # The kind decides the rows
 *
 * `org.Role.humanForbidden` refuses a model chain, a budget and `mcp_env` on
 * a human seat, so those cards are ABSENT for a person rather than drawn
 * with fallbacks ("default provider" is a model for a seat that runs none).
 */

import { useMemo, type ReactNode } from "react";
import { ButtonLink, Callout, Card, EmptyState } from "@crewlethq/ui";
import { CpuGlyph, KeyGlyph, PencilGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { PropertiesRail, type Property } from "~/app/frame/PropertiesRail.tsx";
import { WindowMeters } from "~/components/budget.tsx";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { configValueKind, fmtCount } from "~/lib/format.ts";
import { handleLabel, llmChain, mcpEnvOf, type Seat, type SeatSetup } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import type { AgentRow, HumanContactKey } from "~/protocol/index.ts";
import { ConfigValue, ModelChain, SettingsState } from "./shared.tsx";

/**
 * What a credential variable holds, in words — never its value.
 *
 * `reference` is a whole `${NAME}`: the value is resolved from the secret
 * store when the seat's MCP child is built. `hidden` is the engine's mask, or
 * anything that is not one whole reference, which a credential field shows
 * as set and nothing more.
 */
export function credentialState(value: string | undefined): string {
  switch (configValueKind(value, { secret: true })) {
    case "reference":
      return "from the secret store";
    case "hidden":
    case "literal":
      return "set on the seat (hidden)";
    default:
      return "not set";
  }
}

/**
 * What each contact identity is called, as a person names it — `contact`'s
 * keys are the chart's own (`HumanContactKey`, `internal/org/role.go`), and
 * printed with their underscores swapped for spaces they read "slack user id"
 * for a Slack MEMBER id.
 *
 * NO OPERATOR ID. A contact block says how to reach a person, not which
 * credential they hold: the identity directory binds a credential to a seat,
 * and the key that once did it here is gone with the binding it named.
 */
const CONTACT_LABELS: Readonly<Record<HumanContactKey, string>> = {
  slack_user_id: "Slack member id",
  mattermost_user_id: "Mattermost user id",
  atlassian_account_id: "Atlassian account id",
  github_login: "GitHub login",
  gitlab_username: "GitLab username",
};

/** A contact key's label: the table's, or the key in sentence case for one a newer engine added. */
export function contactLabel(key: string): string {
  const known = (CONTACT_LABELS as Readonly<Record<string, string>>)[key];
  if (known) return known;
  const words = key.replace(/_/g, " ");
  return `${words.charAt(0).toUpperCase()}${words.slice(1)}`;
}

export function Settings({
  seat,
  agent,
  setup,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  /** The profile's one read of the seat's chart row ([useSeatSetup]): asked
   *  again here, the tab read the seat and its home unit twice. */
  setup: SeatSetup;
}) {
  const human = seat.kind === "human";
  const { reading } = setup;
  const row = reading.state === "read" ? reading.seat : null;
  const runtime = row?.runtime;
  // ITS UNIT'S, WITH THE SEAT'S OWN WINNING per variable — and none for a
  // person, who runs no tools (`mcpEnvOf`).
  const credentials = useMemo(() => mcpEnvOf(reading, seat.kind), [reading, seat.kind]);
  const edit = href(["agents", "edit"], { seat: seat.handle || seat.name });
  // THE COMPANY'S CEILINGS, as written on the public chart: a seat with none
  // of its own in a window is still bound by the company's there.
  const company = useOrg()?.token_budget;
  const state = (children: ReactNode) => (
    <SettingsState reading={reading} seat={seat}>
      {children}
    </SettingsState>
  );
  const identity: Property[] = [
    {
      label: "Handle",
      value: seat.handle ? <code className="inline">{handleLabel(seat.handle)}</code> : undefined,
    },
    { label: "Kind", value: human ? "person — never run by the engine" : "agent" },
    // SEALED BY THE CHART: the row carries the `${VAR}` reference that names
    // the address in the secret store, or the mask — never the address.
    { label: "Email", value: <ConfigValue value={row?.email} /> },
    ...Object.entries(runtime?.contact ?? {}).map(([k, v]) => ({
      label: contactLabel(k),
      // NOT A CREDENTIAL: a contact identity is a public handle at a vendor
      // — a Slack member id, a GitHub login — so the literal is the value.
      value: <ConfigValue value={v} />,
    })),
  ];
  return (
    <div className="col gap-4">
      <div className="row gap-2 wrap">
        <p className="t-caption prof-lede">
          From the org chart. Every value here is changed in the org editor, which writes the change
          to the chart.
        </p>
        <span className="spacer" />
        <ButtonLink
          size="small"
          variant="secondary"
          href={edit}
          leadingIcon={<PencilGlyph size="sm" />}
        >
          Edit in org
        </ButtonLink>
      </div>

      <Card>
        <Card.Header icon={<UserGlyph size="sm" />}>
          <Card.Title as="h3">Identity</Card.Title>
        </Card.Header>
        {state(
          <div className="col gap-3">
            <PropertiesRail groups={[{ properties: identity }]} />
            {/* ADMITTED, AND SAID: the engine lets a human seat hold no
                contact identity (a person who works only through the
                dashboard has no chat account to declare), and the chart
                check reports it as `seat_unreachable`. "Needs at least one"
                was a rule the engine does not have. */}
            {human && Object.keys(runtime?.contact ?? {}).length === 0 && (
              <Callout variant="warning">
                A person with no contact identity is reached through the dashboard only: no agent
                can @-mention them, and their activity on other surfaces is not attributed to them.
              </Callout>
            )}
          </div>,
        )}
      </Card>

      {!human && (
        <Card>
          <Card.Header icon={<CpuGlyph size="sm" />}>
            <Card.Title as="h3">Model and budget</Card.Title>
          </Card.Header>
          {state(
            <div className="col gap-3">
              <PropertiesRail
                groups={[
                  {
                    properties: [
                      {
                        label: "Model",
                        // WHAT THE SEAT RUNS ON when it writes no chain of
                        // its own is the company's default, RESOLVED — the
                        // org projection carries the chain a turn walks to a
                        // `config:read` reader, which this card's is.
                        value: llmChain(runtime?.llm).length ? (
                          <ModelChain keys={llmChain(runtime?.llm)} />
                        ) : (seat.raw.llm?.["execute"] ?? []).length ? (
                          <span className="row gap-2 wrap">
                            <span className="muted">the company&rsquo;s default:</span>
                            <ModelChain keys={seat.raw.llm?.["execute"] ?? []} />
                          </span>
                        ) : (
                          <span className="muted">the company&rsquo;s default provider</span>
                        ),
                      },
                      {
                        label: "Auxiliary model",
                        value: llmChain(runtime?.llm_auxiliary).length ? (
                          <ModelChain keys={llmChain(runtime?.llm_auxiliary)} />
                        ) : (
                          <span className="muted">none — reflection uses the model above</span>
                        ),
                      },
                      {
                        label: "Learning",
                        // THREE-VALUED: unset is the company's default, which
                        // is neither of the other two.
                        // A DEFAULTED VALUE IN THE QUIET TONE every other
                        // unset row on this card takes.
                        value:
                          runtime?.learning_enabled == null ? (
                            <span className="muted">the company&rsquo;s default</span>
                          ) : runtime.learning_enabled ? (
                            "on"
                          ) : (
                            "off"
                          ),
                      },
                      ...BUDGET_WINDOWS.map(({ period }) => {
                        const limit = runtime?.token_budget?.[period];
                        const theirs = company?.[period];
                        return {
                          label: `${PERIOD_ADJECTIVE[period][0]!.toUpperCase()}${PERIOD_ADJECTIVE[period].slice(1)} budget`,
                          value:
                            typeof limit === "number" ? (
                              `${fmtCount(limit)} tokens`
                            ) : typeof theirs === "number" ? (
                              <span className="muted">
                                no seat cap — the company&rsquo;s {fmtCount(theirs)} applies
                              </span>
                            ) : (
                              <span className="muted">uncapped</span>
                            ),
                        };
                      }),
                    ],
                  },
                ]}
              />
            </div>,
          )}
          {/* THE ENGINE'S METER, under the ceilings the runtime half writes:
              what the fleet's counter holds the seat to and how much of each
              capped window is spent. PUSHED to every reader, so it is drawn
              whether or not the chart's guarded half could be read. */}
          {agent?.budget?.windows?.length ? (
            <div className="prof-meters">
              <WindowMeters windows={agent.budget.windows} whose={`${seat.name}'s`} />
            </div>
          ) : null}
        </Card>
      )}

      {!human && (
        <Card>
          <Card.Header
            icon={<KeyGlyph size="sm" />}
            subtitle="its unit's, with this seat's own entries winning"
            count={Object.keys(credentials).length}
          >
            <Card.Title as="h3">Tool credentials</Card.Title>
          </Card.Header>
          {state(
            Object.keys(credentials).length ? (
              <div className="col gap-3">
                {Object.entries(credentials).map(([server, vars]) => (
                  <div key={server} className="col gap-1">
                    <div className="t-label">{server}</div>
                    <PropertiesRail
                      groups={[
                        {
                          properties: Object.entries(vars).map(([name, value]) => ({
                            label: name,
                            identifierLabel: true,
                            value: <span className="muted">{credentialState(value)}</span>,
                          })),
                        },
                      ]}
                    />
                  </div>
                ))}
              </div>
            ) : (
              <EmptyState
                size="compact"
                icon={<KeyGlyph size={32} />}
                title="No per-seat tool credentials"
                description="This seat's tools run with whatever the shared MCP servers were configured with."
              />
            ),
          )}
        </Card>
      )}

      {(seat.backstory || seat.guidelines.length > 0) && (
        <Card>
          <Card.Header>
            <Card.Title as="h3">{human ? "About them" : "Prompt profile"}</Card.Title>
          </Card.Header>
          <div className="col gap-3">
            {seat.backstory && (
              <div className="col gap-1">
                <div className="t-label">Backstory</div>
                <p className="t-body measure">{seat.backstory}</p>
              </div>
            )}
            {seat.guidelines.length > 0 && (
              <div className="col gap-1">
                <div className="t-label">Behavioural guidelines</div>
                <ul className="prof-about-list">
                  {seat.guidelines.map((g, i) => (
                    <li key={i}>{g}</li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        </Card>
      )}
    </div>
  );
}
