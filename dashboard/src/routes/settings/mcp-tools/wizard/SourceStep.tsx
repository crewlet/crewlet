/**
 * Where a new server comes from, as four short cards: the curated catalogue,
 * an address the engine checks, a pasted configuration, or a community
 * listing nobody here has reviewed.
 *
 * - CATALOGUE: what the company runs already stands apart in one "Added"
 *   list, a row each with its live state and Open, so it is never offered
 *   twice and never greyed out; what is left is the choice.
 * - ADDRESS: one field with its Check beside it and a few examples. While
 *   the engine checks it (`POST /mcp-probe`) the steps it walks say so; when
 *   it answers, one card says what it found: the server, its transport, the
 *   sign-in it asks for and its tools, or why it could not be reached.
 * - JSON: a code field that names the format as it is typed and lists the
 *   servers it found, each one choosable; a file dropped on it is read the
 *   same way, never run.
 * - COMMUNITY: a slim note, suggested searches, and result cards that each
 *   say "Not reviewed" with who publishes it and when it changed. A search
 *   is sent when asked for, never on each key: each one is a request the
 *   engine makes to the registry.
 */

import { useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  ChoiceCards,
  EntityList,
  FileDrop,
  FilePickButton,
  Input,
  SourceEditor,
  Spinner,
  StatusDot,
  StepList,
  Tag,
  Text,
  type ChoiceCardOption,
  type PickedFile,
} from "@crewlethq/ui";
import {
  FileCodeGlyph,
  GlobeGlyph,
  LayoutGridGlyph,
  SearchGlyph,
  TriangleAlertGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type { McpCatalogueEntry, McpMemberServerStatus, McpProbeAnswer } from "~/contract/mcp.ts";
import { fmtDate } from "~/lib/format.ts";
import { useRest } from "~/lib/useRest.ts";
import { probeAddress, readCatalogue } from "../api.ts";
import { ServerMark } from "../parts.tsx";
import { failureWords, SIGN_IN, STATE } from "../words.ts";
import {
  fromCatalogue,
  nameFrom,
  nameFromAddress,
  newRow,
  readPasted,
  type Draft,
  type PastedFormat,
  type Source,
} from "./draft.ts";

type Set = (patch: Partial<Draft>) => void;

const FORMAT_WORDS: Record<PastedFormat, string> = {
  mcpServers: "mcpServers block",
  servers: "VS Code servers",
  "server.json": "Registry server.json",
};

/**
 * Why a catalogue entry cannot be added here: it signs in with OAuth alone,
 * which this wizard does not set up (its `oauth` block is written in the
 * configuration). Null where it can.
 */
export function unavailableReason(entry: McpCatalogueEntry): string | null {
  const usable = entry.auth.some((a) => a !== "oauth") || entry.auth.length === 0;
  return usable
    ? null
    : `${entry.title} signs in with OAuth alone, which this wizard does not set up.`;
}

/** The sign-ins an entry offers, in a few words. */
function signInWords(e: McpCatalogueEntry): string {
  return e.auth.length > 0 ? e.auth.map((a) => SIGN_IN[a].label).join(" or ") : "Not declared";
}

/** A catalogue or community entry as a compact choice card. */
function entryCard(e: McpCatalogueEntry): ChoiceCardOption {
  const reason = unavailableReason(e);
  return {
    value: e.id,
    label: e.title,
    media: <ServerMark server={{ vendor: e.vendor, transport: e.transport }} size="lg" />,
    description: e.description,
    // The card's own pill in the warning ink, never a Tag inside it.
    ...(e.community ? { tag: "Not reviewed", tagTone: "warning" as const } : {}),
    facts: e.community
      ? [
          { label: "By", value: e.publisher || "Unknown" },
          { label: "Kind", value: e.transport === "http" ? "Remote" : "Local" },
          ...(e.updated_at ? [{ label: "Updated", value: fmtDate(e.updated_at) }] : []),
        ]
      : [
          { label: "Where", value: e.transport === "http" ? "Remote" : "Local" },
          { label: "Sign-in", value: signInWords(e) },
        ],
    ...(reason ? { disabled: true, disabledReason: reason } : {}),
  };
}

export function SourceStep({
  draft,
  set,
  servers,
}: {
  draft: Draft;
  set: Set;
  /** The servers the company runs, so the catalogue shows them as added. */
  servers: readonly McpMemberServerStatus[];
}) {
  const catalogue = useRest("mcp-catalogue:", (signal) => readCatalogue("", signal));
  const curated = catalogue.data?.curated ?? [];
  const pick = (entry: McpCatalogueEntry | undefined) => {
    if (entry) set(fromCatalogue(entry, draft));
  };
  return (
    <>
      <ChoiceCards
        label="Where it comes from"
        minCardWidth="xs"
        density="compact"
        value={draft.source}
        onChange={(v) => set({ source: v as Source })}
        options={[
          {
            value: "catalogue",
            label: "Catalogue",
            media: <LayoutGridGlyph size="lg" />,
            description: "Vetted servers",
          },
          {
            value: "url",
            label: "Address",
            media: <GlobeGlyph size="lg" />,
            description: "Any remote URL",
          },
          {
            value: "json",
            label: "JSON",
            media: <FileCodeGlyph size="lg" />,
            description: "Paste a config",
          },
          {
            value: "community",
            label: "Community",
            media: <UsersGlyph size="lg" />,
            description: "Not reviewed",
          },
        ]}
      />
      {draft.source === "catalogue" ? (
        catalogue.error && !catalogue.data ? (
          <Callout variant="danger" title="The catalogue could not be read">
            {catalogue.error.sentence || catalogue.error.detail}
          </Callout>
        ) : (
          <Curated
            draft={draft}
            curated={curated}
            servers={servers}
            onPick={(id) => pick(curated.find((e) => e.id === id))}
          />
        )
      ) : null}
      {draft.source === "url" ? <AddressCheck draft={draft} set={set} curated={curated} /> : null}
      {draft.source === "json" ? <PastedConfig set={set} /> : null}
      {draft.source === "community" ? <Community draft={draft} onPick={pick} /> : null}
    </>
  );
}

// ---- catalogue -----------------------------------------------------------------

function Curated({
  draft,
  curated,
  servers,
  onPick,
}: {
  draft: Draft;
  curated: readonly McpCatalogueEntry[];
  servers: readonly McpMemberServerStatus[];
  onPick: (id: string) => void;
}) {
  const added = servers.filter((s) => s.configured && s.catalogue);
  const addedIds = new Set(added.map((s) => s.catalogue));
  return (
    <div className="mcp-stack">
      {added.length > 0 ? (
        <section className="mcp-field-group" aria-label="Added">
          <Text as="p" variant="label">
            Added
          </Text>
          <EntityList
            label="Added"
            items={added.map((s) => ({
              id: s.name,
              name: s.title,
              avatar: <ServerMark server={s} />,
              fact: <StatusDot tone={STATE[s.state].tone}>{STATE[s.state].label}</StatusDot>,
              action: (
                <ButtonLink
                  size="small"
                  variant="ghost"
                  href={href(["settings", "mcp-tools", "servers", s.name])}
                  aria-label={`Open ${s.title}`}
                >
                  Open
                </ButtonLink>
              ),
            }))}
          />
        </section>
      ) : null}
      {/* THE TWO GROUPS IN ONE REGISTER, each under the form's own label:
          a card title over one and a micro-label over the other read as two
          kinds of thing. */}
      <section className="mcp-field-group" aria-label="Available">
        {added.length > 0 ? (
          <Text as="p" variant="label">
            Available
          </Text>
        ) : null}
        <ChoiceCards
          label="Server"
          minCardWidth="sm"
          density="compact"
          value={draft.catalogue && !draft.catalogue.community ? draft.catalogue.id : null}
          onChange={onPick}
          options={curated.filter((e) => !addedIds.has(e.id)).map(entryCard)}
        />
      </section>
    </div>
  );
}

// ---- address -------------------------------------------------------------------

/** What the address check walks, in the order it says them while it runs. */
const CHECKS = ["Reach the address", "Discover the server", "Read its sign-in metadata"] as const;

const EXAMPLES = ["mcp.linear.app/mcp", "mcp.stripe.com", "mcp.cloudflare.com/mcp"];

type Probe =
  | { kind: "idle" }
  | { kind: "checking" }
  | { kind: "answered"; answer: McpProbeAnswer }
  | { kind: "refused"; message: string };

function AddressCheck({
  draft,
  set,
  curated,
}: {
  draft: Draft;
  set: Set;
  curated: readonly McpCatalogueEntry[];
}) {
  const [probe, setProbe] = useState<Probe>({ kind: "idle" });
  const run = async (url: string) => {
    setProbe({ kind: "checking" });
    const answer = await probeAddress(url);
    if (answer.kind !== "ok") {
      setProbe({ kind: "refused", message: answer.sentence });
      return;
    }
    const found = answer.value;
    setProbe({ kind: "answered", answer: found });
    if (!found.reachable) return;
    // WHAT THE SERVER SAID about signing in chooses the method, and the step says so.
    const entry = curated.find((e) => e.id === found.catalogue) ?? null;
    const named = entry?.name ?? nameFrom(found.check.server_info?.name ?? "") ?? "";
    set({
      catalogue: entry,
      supported: entry?.auth ?? null,
      detected: found.auth
        ? { auth: found.auth, from: "server", title: found.title || null }
        : null,
      ...(entry?.integration
        ? { signIn: "integration" as const, shared: false }
        : found.auth === "headers"
          ? {
              signIn: "headers" as const,
              ...(draft.headers.length === 0
                ? {
                    headers: entry
                      ? fromCatalogue(entry, draft).headers
                      : [newRow("Authorization", { required: true })],
                  }
                : {}),
            }
          : { signIn: "none" as const }),
      ...(draft.name ? {} : { name: named || nameFromAddress(url) }),
    });
  };
  const failed =
    probe.kind === "answered" && !probe.answer.reachable && probe.answer.check.failure
      ? failureWords(probe.answer.check.failure.class, probe.answer.check.failure.said)
      : null;
  const submit = () => {
    const url = draft.url.trim();
    if (url) void run(/^[a-z]+:\/\//i.test(url) ? url : `https://${url}`);
  };
  return (
    <div className="mcp-stack">
      <form
        className="mcp-address"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <Input
          aria-label="Server address"
          className="mono"
          value={draft.url}
          placeholder="https://mcp.example.com/mcp"
          autoComplete="off"
          spellCheck={false}
          leading={<GlobeGlyph size="sm" />}
          onChange={(e) => {
            setProbe({ kind: "idle" });
            set({ url: e.target.value, transport: "http", catalogue: null, detected: null });
          }}
        />
        <Button
          type="submit"
          variant="primary"
          disabled={!draft.url.trim() || probe.kind === "checking"}
        >
          Check
        </Button>
      </form>
      {probe.kind === "idle" && !draft.url.trim() ? (
        <div className="mcp-chips" aria-label="Examples">
          <Text as="span" variant="caption" tone="secondary">
            Examples
          </Text>
          {EXAMPLES.map((ex) => (
            <Tag
              key={ex}
              size="sm"
              appearance="outline"
              monospace
              onClick={() => {
                set({ url: `https://${ex}`, transport: "http", catalogue: null, detected: null });
                void run(`https://${ex}`);
              }}
            >
              {ex}
            </Tag>
          ))}
        </div>
      ) : null}
      {probe.kind === "checking" ? (
        <StepList
          label="Checking the address"
          aria-live="polite"
          steps={CHECKS.map((c, i) => ({
            id: c,
            label: c,
            state: i === 0 ? "running" : "pending",
          }))}
        />
      ) : null}
      {/* A CHECK THAT FAILED IS A DANGER CALLOUT, as every other failure in
          the flow is (the test, the review's refusal): one look for one
          meaning, rather than a found card with a red edge. */}
      {probe.kind === "refused" ? (
        <Callout variant="danger" title="The address was not checked" live="polite">
          {probe.message}
        </Callout>
      ) : null}
      {probe.kind === "answered" && failed ? (
        <Callout variant="danger" title={failed.title} live="polite">
          {failed.remedy}
        </Callout>
      ) : probe.kind === "answered" && probe.answer.reachable ? (
        <Found answer={probe.answer} />
      ) : null}
    </div>
  );
}

/** What the address check found: the server, how it is reached and signed in to, and its tools. */
function Found({ answer }: { answer: McpProbeAnswer }) {
  const info = answer.check.server_info;
  const oauth = answer.oauth;
  const blocked = oauth?.failure ? failureWords(oauth.failure.class, oauth.failure.said) : null;
  return (
    <div className="mcp-stack" aria-live="polite">
      <div className="mcp-found">
        {/* The mark is centred on the name and the line under it, as a table
            row centres its mark; the facts run under the words. */}
        <span className="mcp-found__mark">
          <ServerMark server={{ vendor: answer.vendor, transport: "http" }} />
        </span>
        <div className="mcp-found__body">
          <span className="mcp-found__title">
            {answer.title || info?.title || info?.name || "An MCP server"}
          </span>
          <Text as="span" variant="caption" tone="secondary">
            {info
              ? `Reported as ${[info.name, info.version].filter(Boolean).join(" ")}`
              : "It asks for a sign-in before it says who it is"}
          </Text>
        </div>
        <dl className="mcp-found__facts">
          <div>
            <dt>Transport</dt>
            <dd>Streamable HTTP</dd>
          </div>
          <div>
            <dt>Sign-in</dt>
            <dd>
              {answer.auth === "oauth"
                ? `OAuth${oauth?.client === "cimd" ? ", published identity" : oauth?.client === "dcr" ? ", registers automatically" : ""}`
                : answer.auth
                  ? SIGN_IN[answer.auth].label
                  : "Not said"}
            </dd>
          </div>
          <div>
            <dt>Tools</dt>
            <dd>{answer.tools === null ? "Listed after sign-in" : answer.tools}</dd>
          </div>
        </dl>
      </div>
      {blocked ? (
        <Callout variant="warning" title={blocked.title}>
          {blocked.remedy}
        </Callout>
      ) : null}
    </div>
  );
}

// ---- pasted configuration ---------------------------------------------------------

const EXAMPLE = `{
  "mcpServers": {
    "sentry": {
      "url": "https://mcp.sentry.dev/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}`;

function PastedConfig({ set }: { set: Set }) {
  const [text, setText] = useState("");
  const [chosen, setChosen] = useState<string | null>(null);
  const read = readPasted(text);
  // THE FIRST SERVER FOUND IS TAKEN as the text is read, so a paste of one
  // server needs no second press; choosing another below takes that one.
  const take = (next: string) => {
    setText(next);
    const found = readPasted(next);
    if (found.state !== "read") return;
    const server = found.servers.find((s) => s.name === chosen) ?? found.servers[0]!;
    setChosen(server.name);
    set({ ...server.draft });
  };
  const onFiles = async (files: PickedFile[]) => {
    const f = files.find((x) => x.path.endsWith(".json")) ?? files[0];
    if (f) take(await f.file.text());
  };
  return (
    <div className="mcp-stack">
      <FileDrop onFiles={(f) => void onFiles(f)} title="Drop a JSON file to read it">
        <div className="mcp-code">
          <div className="mcp-code__bar">
            <Text as="span" variant="caption" tone="secondary">
              Server configuration
            </Text>
            {read.state === "read" ? (
              <Tag size="xs" variant="success" appearance="soft">
                {FORMAT_WORDS[read.format]}
              </Tag>
            ) : read.state === "error" ? (
              <Tag size="xs" variant="neutral" appearance="outline">
                {read.message}
              </Tag>
            ) : null}
            <FilePickButton
              className="mcp-code__pick"
              accept=".json,application/json"
              onFiles={(f) => void onFiles(f)}
              size="small"
              variant="ghost"
            >
              Choose a file
            </FilePickButton>
          </div>
          <SourceEditor
            label="Server configuration"
            value={text}
            onChange={take}
            placeholder={EXAMPLE}
            marks={
              read.state === "error" && read.line
                ? [{ line: read.line, tone: "warning", message: read.message }]
                : []
            }
            className="mcp-code__editor"
          />
        </div>
      </FileDrop>
      <Text as="p" variant="caption" tone="secondary">
        An mcpServers block, a VS Code servers block or a registry server.json. It is read, never
        run, and any value it carries is left out: you type it again where it is stored.
      </Text>
      {read.state === "read" ? (
        <ChoiceCards
          label="Servers found"
          minCardWidth="sm"
          density="compact"
          value={chosen}
          onChange={(name) => {
            const s = read.servers.find((x) => x.name === name);
            if (!s) return;
            setChosen(name);
            set({ ...s.draft });
          }}
          options={read.servers.map((s) => ({
            value: s.name,
            label: s.name,
            media: <ServerMark server={{ vendor: "", transport: s.transport }} size="lg" />,
            description: (
              <Text as="span" className="mono" truncate>
                {s.where}
              </Text>
            ),
            facts: [
              { label: "Kind", value: s.transport === "http" ? "Remote" : "Local" },
              {
                label: s.transport === "http" ? "Headers" : "Variables",
                value: s.keys.length ? s.keys.join(", ") : "None",
              },
            ],
          }))}
        />
      ) : null}
    </div>
  );
}

// ---- community ----------------------------------------------------------------------

const SUGGESTED = ["figma", "browser", "postgres", "notion"];

function Community({
  draft,
  onPick,
}: {
  draft: Draft;
  onPick: (entry: McpCatalogueEntry | undefined) => void;
}) {
  const [typed, setTyped] = useState("");
  const [asked, setAsked] = useState("");
  const search = useRest(asked ? `mcp-catalogue:${asked}` : null, (signal) =>
    readCatalogue(asked, signal),
  );
  const found = search.data?.community ?? [];
  const ask = (q: string) => {
    setTyped(q);
    setAsked(q.trim());
  };
  return (
    <div className="mcp-stack">
      <p className="mcp-slim-note">
        <TriangleAlertGlyph size="sm" />
        <span>
          Community servers come from a public registry and nobody here has reviewed them.
        </span>
      </p>
      <form
        className="mcp-address"
        role="search"
        onSubmit={(e) => {
          e.preventDefault();
          ask(typed);
        }}
      >
        <Input
          aria-label="Search community servers"
          placeholder="Search community servers"
          leading={<SearchGlyph size="sm" />}
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
        />
        <Button type="submit" variant="secondary" disabled={!typed.trim()}>
          Search
        </Button>
      </form>
      {!asked ? (
        <div className="mcp-chips" aria-label="Suggested searches">
          <Text as="span" variant="caption" tone="secondary">
            Try
          </Text>
          {SUGGESTED.map((s) => (
            <Tag key={s} size="sm" appearance="outline" onClick={() => ask(s)}>
              {s}
            </Tag>
          ))}
        </div>
      ) : search.loading ? (
        <div className="mcp-testing" aria-live="polite">
          <Spinner size="sm" />
          <Text as="span" tone="secondary">
            Searching the registry
          </Text>
        </div>
      ) : search.error ? (
        <Callout variant="danger" title="The registry was not searched">
          {search.error.sentence || search.error.detail}
        </Callout>
      ) : found.length > 0 ? (
        <ChoiceCards
          label="Community server"
          minCardWidth="sm"
          density="compact"
          value={draft.catalogue?.community ? draft.catalogue.id : null}
          onChange={(id) => onPick(found.find((e) => e.id === id))}
          options={found.map(entryCard)}
        />
      ) : (
        <Text as="p" tone="secondary">
          {search.data?.reason || `Nothing in the registry matches ${asked}.`}
        </Text>
      )}
    </div>
  );
}
