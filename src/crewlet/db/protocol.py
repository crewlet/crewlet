"""Storage backend protocol."""

from __future__ import annotations

from typing import Any, Protocol, runtime_checkable


@runtime_checkable
class StorageBackend(Protocol):
    """Protocol for storage backends."""

    async def save(self, collection: str, id: str, data: dict[str, Any]) -> None: ...

    async def load(self, collection: str, id: str) -> dict[str, Any] | None: ...

    async def query(
        self, collection: str, filters: dict[str, Any] | None = None
    ) -> list[dict[str, Any]]: ...

    async def delete(self, collection: str, id: str) -> None: ...
