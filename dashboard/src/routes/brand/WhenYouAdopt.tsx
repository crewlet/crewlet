/**
 * What publishing a brand does, said the same way wherever a publish is
 * confirmed: the setup's Review step and the publish dialog.
 *
 * THREE SHORT ITEMS, EACH UNDER ITS GLYPH, in place of the one long paragraph
 * that said all three at once: who sees the brand, what switches now, and what
 * switches later or never.
 */

import type { ReactNode } from "react";
import { List, ListItem, Section, Text } from "@crewlethq/ui";
import { LayersGlyph, RefreshCwGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";

/** The items, in the order they are read. Exported for the tests that check the words. */
export const WHEN_YOU_ADOPT: readonly { key: string; icon: ReactNode; text: string }[] = [
  {
    key: "visible",
    icon: <UsersGlyph size="sm" />,
    text: "Everyone in the company and every agent can see it, including agents that talk to people outside the company.",
  },
  {
    key: "now",
    icon: <RefreshCwGlyph size="sm" />,
    text: "Docs and dashboards switch to it right away.",
  },
  {
    key: "later",
    icon: <LayersGlyph size="sm" />,
    text: "Pages and designs switch when they are next published. Existing decks keep their look.",
  },
];

export function WhenYouAdopt() {
  return (
    <Section title="When you publish" spacing="tight" className="brand-when-you-adopt">
      <List variant="plain" density="compact">
        {WHEN_YOU_ADOPT.map((item) => (
          <ListItem key={item.key} leading={item.icon}>
            <Text as="span" variant="body">
              {item.text}
            </Text>
          </ListItem>
        ))}
      </List>
    </Section>
  );
}
