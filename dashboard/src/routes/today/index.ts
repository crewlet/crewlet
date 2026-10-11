/**
 * Today's chunk: a person's own first screen. See `app/lazyScreen.ts`.
 * Nothing outside `app/` imports this file — a static import of it from the
 * frame would pull the workspace back into the entry chunk.
 */

export { Today } from "./Today.tsx";
