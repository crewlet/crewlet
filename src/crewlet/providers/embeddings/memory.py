"""In-memory mock embedding provider for dev/testing."""

from __future__ import annotations

import hashlib
import math


class MemoryEmbeddingProvider:
    """Deterministic hash-based embedding provider.

    Generates pseudo-embeddings by hashing input text, suitable for
    development and testing without an external embedding service.
    """

    def __init__(self, dimensions: int = 1536) -> None:
        if dimensions < 1:
            raise ValueError(f"dimensions must be >= 1, got {dimensions}")
        self._dimensions = dimensions

    @property
    def dimensions(self) -> int:
        return self._dimensions

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [self._hash_embed(text) for text in texts]

    def _hash_embed(self, text: str) -> list[float]:
        """Generate a deterministic, normalized vector from text."""
        raw: list[float] = []
        # Generate enough hash bytes to fill the dimensions
        i = 0
        while len(raw) < self._dimensions:
            h = hashlib.sha256(f"{text}:{i}".encode()).digest()
            for byte in h:
                if len(raw) >= self._dimensions:
                    break
                # Map byte to [-1, 1]
                raw.append((byte / 127.5) - 1.0)
            i += 1
        # L2-normalize
        norm = math.sqrt(sum(x * x for x in raw))
        if norm > 0:
            raw = [x / norm for x in raw]
        return raw
