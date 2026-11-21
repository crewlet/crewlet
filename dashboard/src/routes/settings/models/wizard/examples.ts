/**
 * What the wizard shows as an example of a model id or a key: the vendor the
 * draft's harness, method and provider send the model's calls to, and that
 * vendor's own spelling. The one table every placeholder and hint reads, so no
 * step names one vendor's model while another vendor is chosen.
 *
 * AN EXAMPLE IS GIVEN ONLY WHERE IT IS KNOWN TO BE THE VENDOR'S OWN: a vendor
 * with no example here is named instead ("The model's id at Groq").
 */

import { CLOUD_FIELDS, namesProvider, providersFor, type Draft } from "./draft.ts";

/** One vendor's spelling: an example model id, and its key's and token's prefixes. */
interface Vendor {
  label: string;
  model?: string;
  key?: string;
  token?: string;
}

const VENDORS: Readonly<Record<string, Vendor>> = {
  anthropic: {
    label: "Anthropic",
    model: "claude-sonnet-5-5",
    key: "sk-ant-api03-…",
    token: "sk-ant-oat01-…",
  },
  openai: { label: "OpenAI", model: "gpt-5.5", key: "sk-…" },
  openrouter: { label: "OpenRouter", model: "qwen/qwen3-coder", key: "sk-or-v1-…" },
  groq: { label: "Groq", key: "gsk_…" },
  gemini: { label: "Google AI Studio", key: "AIza…" },
  google: { label: "Google", key: "AIza…" },
  xai: { label: "xAI", key: "xai-…" },
  deepseek: { label: "DeepSeek", key: "sk-…" },
  github: { label: "GitHub", key: "github_pat_…" },
};

/** The vendor a harness that fronts no providers always sends its calls to. */
const HARNESS_VENDOR: Readonly<Record<string, string>> = {
  "claude-code": "anthropic",
  codex: "openai",
  "gemini-cli": "google",
  grok: "xai",
  copilot: "github",
};

/** The vendor the draft's calls go to, and its label: the provider chosen, the cloud, or the harness's own. */
function vendorOf(d: Pick<Draft, "harness" | "method" | "provider" | "cloud">): Vendor | null {
  if (d.method === "cloud") return { label: CLOUD_FIELDS[d.cloud].label };
  const providers = providersFor(d.harness);
  if (providers.length > 0) {
    const chosen = providers.find((p) => p.value === d.provider);
    return VENDORS[d.provider] ?? (chosen ? { label: chosen.label } : null);
  }
  return VENDORS[HARNESS_VENDOR[d.harness] ?? ""] ?? null;
}

/**
 * The model field's placeholder and the hint on how the id is sent, from the
 * same vendor: OpenCode's and Pi's id names the chosen provider first.
 */
export function modelExample(d: Pick<Draft, "harness" | "method" | "provider" | "cloud">): {
  placeholder: string;
  naming: string;
} {
  const vendor = vendorOf(d);
  return {
    placeholder: vendor?.model ?? (vendor ? `The model's id at ${vendor.label}` : "The model's id"),
    naming: namesProvider(d.harness)
      ? `Sent as ${d.provider}/<model>.`
      : "In the harness's own naming.",
  };
}

/** The credential field's placeholder: the vendor's own prefix for a key or a token where it has one. */
export function credentialExample(
  d: Pick<Draft, "harness" | "method" | "provider" | "cloud">,
): string {
  const vendor = vendorOf(d);
  const prefix = d.method === "token" ? vendor?.token : vendor?.key;
  return prefix ?? (d.method === "token" ? "Paste the token" : "Paste the key");
}
