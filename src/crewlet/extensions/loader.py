"""Extension loader — discovers, registers, and manages extensions."""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.extensions.protocol import Extension, ExtensionContext

logger = get_logger("extensions")


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
            await extension.on_register(ctx)
        except Exception as exc:
            logger.exception(
                "extension_register_failed", extension=extension.name, error=str(exc)
            )
            raise
        self._extensions.append(extension)
        logger.info("extension_registered", extension=extension.name)

    async def start_all(self, ctx: ExtensionContext) -> None:
        """Call on_engine_start for all registered extensions."""
        logger.debug("extensions_starting", count=len(self._extensions))
        failed: list[str] = []
        for ext in self._extensions:
            try:
                await ext.on_engine_start(ctx)
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
                await ext.on_engine_stop(ctx)
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
            await extension.on_engine_stop(ctx)
        except Exception as exc:
            logger.exception(
                "extension_stop_failed", extension=extension.name, error=str(exc)
            )
        # Already absent → nothing to remove; swallow ValueError.
        import contextlib

        with contextlib.suppress(ValueError):
            self._extensions.remove(extension)
        logger.info("extension_unregistered", extension=extension.name)
