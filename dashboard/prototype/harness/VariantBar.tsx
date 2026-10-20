/**
 * The variant bar: the one control a prototype adds to the dashboard.
 *
 * It names the topic, the variant and the scenario on screen, says in one
 * line what that variant proposes, switches to another variant (keeping the
 * address, so the same screen opens in the next one), switches the demo
 * company's scenario, and resets the demo. `[` and `]` step through the
 * variants from the keyboard. Folded, it is a pill naming the variant, so it
 * never covers the screen it is beside.
 *
 * THE VARIANTS HOLD STILL. The card is one width whatever its summary says,
 * and the variants are its last row, starting at its edge: the bar is pinned
 * to the bottom of the window, so a longer summary grows the card upwards
 * and leaves the row where it was. A variant sits at the same place on every
 * page, and a reader clicking along the row lands where they aimed.
 *
 * BUILT FROM `@crewlethq/ui` ALONE, as every surface of the dashboard is: its
 * own stylesheet places it and draws nothing.
 */

import { useCallback, useEffect, useState } from "react";
import {
  Button,
  Card,
  IconButton,
  Kbd,
  Menu,
  SegmentedControl,
  Text,
  Tooltip,
  isComposing,
  isModalLayerOpen,
  ThemeToggle,
} from "@crewlethq/ui";
import { ChevronDownGlyph, LayersGlyph, RotateCcwGlyph } from "@crewlethq/icons/glyphs";
import { isTyping } from "~/lib/keys.ts";
import { useTheme } from "~/lib/prefs.ts";
import type { Topic, Variant } from "./defineTopic.ts";
import { barFolded, setBarFolded } from "./session.ts";

export interface VariantBarProps {
  topic: Topic;
  variant: Variant;
  /** The scenario on screen, by id. */
  scenario: string;
  /** Switch the scenario: the page reloads into it. */
  onScenario: (id: string) => void;
  /** Start the demo over from the scenario on screen, as written: every change dropped, the scenario kept. */
  onReset: () => void;
  /** Open another variant; by default, its page beside this one, on the same address. */
  onVariant?: (id: string) => void;
}

/** Another variant's page, on the address this one is showing. */
function openVariant(id: string): void {
  location.href = `../${encodeURIComponent(id)}/index.html${location.hash}`;
}

export function VariantBar({
  topic,
  variant,
  scenario,
  onScenario,
  onReset,
  onVariant = openVariant,
}: VariantBarProps) {
  const [folded, setFolded] = useState(barFolded);
  const [theme, setTheme] = useTheme();
  const fold = (next: boolean) => {
    setFolded(next);
    setBarFolded(next);
  };
  const ids = topic.variants.map((v) => v.id);
  const step = useCallback(
    (delta: number) => {
      const at = ids.indexOf(variant.id);
      const next = ids[(at + delta + ids.length) % ids.length];
      if (next && next !== variant.id) onVariant(next);
    },
    [ids, variant.id, onVariant],
  );

  // `[` AND `]` STEP THE VARIANTS, unless the dashboard took the press: the
  // peek rail and the org chart both step their own lists on these keys, and
  // they mark a press they handled. So the decision waits for the press to
  // finish travelling, and reads whether anybody claimed it.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "[" && event.key !== "]") return;
      if (event.metaKey || event.ctrlKey || event.altKey || isComposing(event)) return;
      if (isTyping() || isModalLayerOpen()) return;
      setTimeout(() => {
        if (!event.defaultPrevented) step(event.key === "]" ? 1 : -1);
      }, 0);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [step]);

  if (folded) {
    return (
      <div className="proto-bar" data-folded="true">
        <Tooltip content={`${topic.title}: ${variant.summary}`} placement="top">
          <Button
            size="small"
            shape="pill"
            variant="secondary"
            leadingIcon={<LayersGlyph size="sm" />}
            onClick={() => fold(false)}
          >
            {variant.label}
          </Button>
        </Tooltip>
      </div>
    );
  }

  const current = topic.scenarios.find((s) => s.id === scenario) ?? topic.scenarios[0];
  return (
    <div className="proto-bar">
      <Card
        variant="elevated"
        padding="tight"
        className="proto-bar-card"
        aria-label={`${topic.title} prototype`}
      >
        <div className="proto-bar-head">
          <Text as="span" variant="label" truncate className="proto-bar-title">
            {topic.title}
          </Text>
          <div className="proto-bar-actions">
            <Menu
              label="Scenario"
              trigger={`Scenario: ${current?.label ?? scenario}`}
              triggerVariant="secondary"
              align="end"
              items={topic.scenarios.map((s) => ({
                key: s.id,
                label: s.label,
                checked: s.id === scenario,
                onSelect: () => s.id !== scenario && onScenario(s.id),
              }))}
            />
            {/* THE DASHBOARD'S OWN THEME, so a choice here is the one its
                Appearance settings show, and it holds across every variant. */}
            <ThemeToggle value={theme} onChange={setTheme} variant="ghost" size="md" />
            <Tooltip
              content="Start over: forget every change made in this tab; the scenario stays"
              placement="top"
            >
              <IconButton
                label="Reset the demo"
                icon={<RotateCcwGlyph size="sm" />}
                onClick={onReset}
              />
            </Tooltip>
            <Tooltip content="Fold the bar to a pill" placement="top">
              <IconButton
                label="Fold the variant bar"
                icon={<ChevronDownGlyph size="sm" />}
                onClick={() => fold(true)}
              />
            </Tooltip>
          </div>
        </div>
        <div className="proto-bar-foot">
          <Text as="p" variant="caption" tone="secondary" className="proto-bar-summary">
            {variant.summary}
          </Text>
          {ids.length > 1 ? (
            <Text as="span" variant="caption" tone="tertiary" className="proto-bar-keys">
              <Kbd subtle>[</Kbd> <Kbd subtle>]</Kbd> to switch
            </Text>
          ) : null}
        </div>
        <SegmentedControl
          label="Variant"
          layout="chips"
          semantics="radio"
          activate="manual"
          activateHint={null}
          size="sm"
          className="proto-bar-variants"
          options={topic.variants.map((v) => ({ value: v.id, label: v.label, title: v.summary }))}
          value={variant.id}
          onValueChange={(id) => id !== variant.id && onVariant(id)}
        />
      </Card>
    </div>
  );
}
