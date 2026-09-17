/**
 * The stylesheet entry points of `@crewlethq/tokens`.
 *
 * Its export map publishes them without a file extension (`./css`,
 * `./css/themes`), and the `*.css` declaration `vite/client` provides matches
 * on the extension, so TypeScript refuses each side-effect import as a module
 * it has no declaration for. The bundler resolves them perfectly well; this
 * only tells the compiler they exist.
 *
 * The fix belongs upstream, as a `types` condition on those export entries.
 * Until the package carries one, this is the one declaration the engine keeps
 * for it, and it says nothing about the modules beyond their existence.
 */

declare module "@crewlethq/tokens/css";
declare module "@crewlethq/tokens/css/*";
