"""Tests for the memory embedding provider."""

from __future__ import annotations

import math

import pytest

from crewlet.providers.embeddings.memory import MemoryEmbeddingProvider
from crewlet.providers.embeddings.protocol import EmbeddingProvider


class TestMemoryEmbeddingProvider:
    def test_implements_protocol(self) -> None:
        provider = MemoryEmbeddingProvider()
        assert isinstance(provider, EmbeddingProvider)

    def test_default_dimensions(self) -> None:
        provider = MemoryEmbeddingProvider()
        assert provider.dimensions == 1536

    def test_custom_dimensions(self) -> None:
        provider = MemoryEmbeddingProvider(dimensions=128)
        assert provider.dimensions == 128

    async def test_embed_returns_correct_shape(self) -> None:
        provider = MemoryEmbeddingProvider(dimensions=64)
        texts = ["hello", "world"]
        result = await provider.embed(texts)
        assert len(result) == 2
        assert all(len(vec) == 64 for vec in result)

    async def test_embed_deterministic(self) -> None:
        provider = MemoryEmbeddingProvider(dimensions=32)
        a = await provider.embed(["test"])
        b = await provider.embed(["test"])
        assert a == b

    async def test_embed_different_texts_differ(self) -> None:
        provider = MemoryEmbeddingProvider(dimensions=32)
        result = await provider.embed(["foo", "bar"])
        assert result[0] != result[1]

    async def test_embed_vectors_are_normalized(self) -> None:
        provider = MemoryEmbeddingProvider(dimensions=128)
        result = await provider.embed(["normalize me"])
        norm = math.sqrt(sum(x * x for x in result[0]))
        assert abs(norm - 1.0) < 1e-6

    async def test_embed_empty_list(self) -> None:
        provider = MemoryEmbeddingProvider()
        result = await provider.embed([])
        assert result == []

    async def test_embed_values_in_range(self) -> None:
        provider = MemoryEmbeddingProvider(dimensions=256)
        result = await provider.embed(["range check"])
        assert all(-1.0 <= x <= 1.0 for x in result[0])

    def test_invalid_dimensions_zero(self) -> None:
        with pytest.raises(ValueError, match="dimensions must be >= 1"):
            MemoryEmbeddingProvider(dimensions=0)

    def test_invalid_dimensions_negative(self) -> None:
        with pytest.raises(ValueError, match="dimensions must be >= 1"):
            MemoryEmbeddingProvider(dimensions=-5)
