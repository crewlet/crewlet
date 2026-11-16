/**
 * The web pages a turn read, cited under its answer: one link card per page,
 * opened only through the kit's interstitial, which names the host first.
 */

import { useState, type MouseEvent } from "react";
import { LinkCard, LinkInterstitial, Stack, Text, externalUrl } from "@crewlethq/ui";
import { GlobeGlyph } from "@crewlethq/icons/glyphs";
import type { AssistantCardEvent, AssistantEntityCard } from "~/contract/assistant.ts";
import { fmtMinute } from "~/lib/format.ts";

/** A page Ask Crewlet read: its address, from the engine's own fetch record, and its title. */
export interface Source {
  url: URL;
  title: string;
  /** When it was read, from the card's `Read` fact. */
  read: string | undefined;
}

/** The web pages among a turn's cards, each once, in the order they were read. */
export function sourcesOf(cards: readonly AssistantCardEvent[]): Source[] {
  const out = new Map<string, Source>();
  for (const event of cards) {
    const entity: AssistantEntityCard | undefined = event.card.entity;
    if (event.card.type !== "entity" || entity?.kind !== "web_page") continue;
    // THE ADDRESS IS THE ENGINE'S, its fetch record's id, never text a model wrote.
    const url = externalUrl(entity.id);
    if (url === null || url.protocol !== "https:") continue;
    const read = entity.facts?.find((f) => f.label === "Read")?.value;
    out.set(url.href, { url, title: entity.title || url.host, read });
  }
  return [...out.values()];
}

// The person opens a source, so the stop says who read it rather than who asks.
const LABELS = {
  intro: () => "Ask Crewlet read this page. Open it only if you trust where it goes.",
};

/** The pages a turn read, as link cards that open through the interstitial. */
export function Sources({ sources }: { sources: readonly Source[] }) {
  const [opening, setOpening] = useState<URL | null>(null);
  if (sources.length === 0) return null;
  return (
    <section aria-label="Sources">
      <Stack gap={2}>
        <Text as="h3" variant="caption" tone="secondary">
          Sources
        </Text>
        {sources.map((s) => (
          <LinkCard
            key={s.url.href}
            href={s.url.href}
            external
            source={{ name: s.url.host, mark: <GlobeGlyph size="xs" /> }}
            title={s.title}
            meta={s.read ? `Read ${fmtMinute(s.read)}` : undefined}
            address={s.url.href}
            onClick={(event: MouseEvent<HTMLAnchorElement>) => {
              event.preventDefault();
              setOpening(s.url);
            }}
          />
        ))}
      </Stack>
      <LinkInterstitial url={opening} labels={LABELS} onClose={() => setOpening(null)} />
    </section>
  );
}
