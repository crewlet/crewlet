"""Extension loader — discovers, registers, and manages extensions."""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.extensions.protocol import Extension, ExtensionContext
from crewlet.tools.registry import ToolRegistry, extension_origin

logger = get_logger("extensions")


def _scoped(ctx: ExtensionContext, extension: Extension) -> ExtensionContext:
    """The context one extension sees: the shared one, with a
    ``tool_registry`` that stamps this extension's name on every tool it
    registers.

    Scoping has to happen here because this is the last frame that knows
    whose hook is about to run.  A tool an extension registers is
    structurally identical to an engine builtin, so an origin not
    recorded at registration cannot be recovered later — the dashboard
    reported extension tools as ``builtin``, and a tool absent because
    its extension failed to load looked like a missing builtin.

    A context carrying no real registry (a bare ``ExtensionContext`` in
    a test) passes through untouched.
    """
    registry = ctx.tool_registry
    if not isinstance(registry, ToolRegistry):
        return ctx
    return ctx.model_copy(
        update={"tool_registry": registry.for_origin(extension_origin(extension.name))}
    )


class ExtensionManager:
    """Manages the lifecycle of registered extensions."""

    def __init__(self) -> None:
        self._extensions: list[Extension] = []

    @property
    def extensions(self) -> list[Extension]:
        return list(self._extensions)

    async def register(self, extension: Extension, ctx: ExtensionContext) -> None:
        """Register an extension and call its on_register hook."""
        logger.debug("extension_registering", extension=extension.name)
        try:
            await extension.on_register(_scoped(ctx, extension))
        except Exception as exc:
            logger.exception(
                "extension_register_failed", extension=extension.name, error=str(exc)
            )
            raise
        self._extensions.append(extension)
        logger.info("extension_registered", extension=extension.name)

    async def start(self, extension: Extension, ctx: ExtensionContext) -> None:
        """Call ``on_engine_start`` for a single extension.

        Used by :meth:`Engine._apply_extensions_live` when a config edit
        adds or restarts one extension on a running engine — the rest
        are already started, so :meth:`start_all` is the wrong verb.
        Goes through the manager rather than calling the hook directly
        so the per-extension context is built the one way.
        """
        await extension.on_engine_start(_scoped(ctx, extension))

    async def start_all(self, ctx: ExtensionContext) -> None:
        """Call on_engine_start for all registered extensions."""
        logger.debug("extensions_starting", count=len(self._extensions))
        failed: list[str] = []
        for ext in self._extensions:
            try:
                await ext.on_engine_start(_scoped(ctx, ext))
                logger.debug("extension_started", extension=ext.name)
            except Exception as exc:
                # Continue starting remaining extensions even if one fails.
                logger.exception(
                    "extension_start_failed", extension=ext.name, error=str(exc)
                )
                failed.append(ext.name)
        if failed:
            logger.warning(
                "extensions_started_with_failures",
                failed=failed,
                total=len(self._extensions),
            )
        else:
            logger.debug("extensions_started", count=len(self._extensions))

    async def stop_all(self, ctx: ExtensionContext) -> None:
        """Call on_engine_stop for all registered extensions."""
        logger.debug("extensions_stopping", count=len(self._extensions))
        for ext in reversed(self._extensions):
            try:
                await ext.on_engine_stop(_scoped(ctx, ext))
                logger.debug("extension_stopped", extension=ext.name)
            except Exception as exc:
                # Continue stopping remaining extensions even if one fails.
                logger.exception(
                    "extension_stop_failed", extension=ext.name, error=str(exc)
                )

    async def unregister(self, extension: Extension, ctx: ExtensionContext) -> None:
        """Stop and drop a single extension.

        Used by :meth:`Engine._apply_extensions_diff` when a live
        config edit removes an extension entry.  Calls
        ``on_engine_stop`` first; the extension is dropped from the
        registry regardless of whether the stop hook raised, so the
        engine doesn't keep referencing a half-disposed extension.
        """
        logger.debug("extension_unregistering", extension=extension.name)
        try:
            await extension.on_engine_stop(_scoped(ctx, extension))
        except Exception as exc:
            logger.exception(
                "extension_stop_failed", extension=extension.name, error=str(exc)
            )
        # Already absent → nothing to remove; swallow ValueError.
        import contextlib

        with contextlib.suppress(ValueError):
            self._extensions.remove(extension)
        logger.info("extension_unregistered", extension=extension.name)
