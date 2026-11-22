import { HARNESS_FACTS } from "~/contract/harnesses.ts";

const hasRunner = (harness: string) => HARNESS_FACTS[harness]?.executor === true;

/**
 * The harnesses that can run a seat's executor, in the profiles' order
 * (`HARNESS_FACTS[..].executor`): a member of an executor chain on any other
 * harness is skipped at launch and serves the text phases alone.
 */
export const EXECUTOR_HARNESSES: readonly string[] = Object.keys(HARNESS_FACTS).filter(hasRunner);
