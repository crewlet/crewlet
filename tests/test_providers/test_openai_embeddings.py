"""Tests for the OpenAI embedding provider."""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from crewlet.providers.embeddings.openai import OpenAIEmbeddingProvider
from crewlet.providers.embeddings.protocol import EmbeddingProvider


class TestOpenAIEmbeddingProvider:
    def test_implements_protocol(self) -> None:
        provider = OpenAIEmbeddingProvider()
        assert isinstance(provider, EmbeddingProvider)

    def test_default_model_and_dimensions(self) -> None:
        provider = OpenAIEmbeddingProvider()
        assert provider.model == "text-embedding-3-small"
        assert provider.dimensions == 1536

    def test_large_model_dimensions(self) -> None:
        provider = OpenAIEmbeddingProvider(
            model="text-embedding-3-large", dimensions=3072
        )
        assert provider.dimensions == 3072

    def test_custom_dimensions_override(self) -> None:
        provider = OpenAIEmbeddingProvider(dimensions=512)
        assert provider.dimensions == 512

    def test_api_key_from_constructor(self) -> None:
        provider = OpenAIEmbeddingProvider(api_key="sk-test")
        assert provider.api_key == "sk-test"

    @patch.dict("os.environ", {"OPENAI_API_KEY": "sk-env"})
    def test_api_key_from_env(self) -> None:
        provider = OpenAIEmbeddingProvider()
        assert provider.api_key == "sk-env"

    def test_custom_base_url(self) -> None:
        provider = OpenAIEmbeddingProvider(base_url="https://custom.api/v1/")
        assert provider.base_url == "https://custom.api/v1"

    async def test_embed_calls_openai_api(self) -> None:
        provider = OpenAIEmbeddingProvider(api_key="sk-test")

        mock_embedding = MagicMock()
        mock_embedding.index = 0
        mock_embedding.embedding = [0.1, 0.2, 0.3]

        mock_usage = MagicMock()
        mock_usage.total_tokens = 5

        mock_response = MagicMock()
        mock_response.data = [mock_embedding]
        mock_response.usage = mock_usage

        provider._client.embeddings.create = AsyncMock(return_value=mock_response)

        result = await provider.embed(["hello world"])

        provider._client.embeddings.create.assert_awaited_once_with(
            model="text-embedding-3-small",
            input=["hello world"],
        )
        assert result == [[0.1, 0.2, 0.3]]

    async def test_embed_preserves_order(self) -> None:
        provider = OpenAIEmbeddingProvider(api_key="sk-test")

        # Return out of order to test sorting
        emb0 = MagicMock(index=1, embedding=[0.4, 0.5])
        emb1 = MagicMock(index=0, embedding=[0.1, 0.2])

        mock_response = MagicMock()
        mock_response.data = [emb0, emb1]
        mock_response.usage = MagicMock(total_tokens=10)

        provider._client.embeddings.create = AsyncMock(return_value=mock_response)

        result = await provider.embed(["first", "second"])

        assert result == [[0.1, 0.2], [0.4, 0.5]]

    async def test_embed_empty_list(self) -> None:
        provider = OpenAIEmbeddingProvider(api_key="sk-test")
        result = await provider.embed([])
        assert result == []


class TestEmbeddingProviderConfig:
    """Tests for embedding provider configuration and factory."""

    def test_config_model(self) -> None:
        from crewlet.config import EmbeddingProviderConfig

        cfg = EmbeddingProviderConfig(
            type="openai",
            model="text-embedding-3-large",
            api_key="sk-test",
            dimensions=3072,
        )
        assert cfg.type == "openai"
        assert cfg.model == "text-embedding-3-large"
        assert cfg.api_key == "sk-test"
        assert cfg.dimensions == 3072

    def test_config_defaults(self) -> None:
        from crewlet.config import EmbeddingProviderConfig

        cfg = EmbeddingProviderConfig()
        assert cfg.type == "openai"
        assert cfg.model == "text-embedding-3-small"
        assert cfg.dimensions == 1536

    def test_create_openai_provider(self) -> None:
        from crewlet.config import EmbeddingProviderConfig, create_embedding_provider

        cfg = EmbeddingProviderConfig(
            type="openai",
            model="text-embedding-3-small",
            api_key="sk-test",
        )
        provider = create_embedding_provider(cfg)
        assert isinstance(provider, OpenAIEmbeddingProvider)
        assert provider.model == "text-embedding-3-small"
        assert provider.api_key == "sk-test"

    def test_create_openai_compatible_provider(self) -> None:
        from crewlet.config import EmbeddingProviderConfig, create_embedding_provider

        cfg = EmbeddingProviderConfig(
            type="openai-compatible",
            model="custom-embed",
            base_url="https://custom.api/v1",
            dimensions=4096,
        )
        provider = create_embedding_provider(cfg)
        assert isinstance(provider, OpenAIEmbeddingProvider)
        assert provider.base_url == "https://custom.api/v1"
        assert provider.dimensions == 4096

    def test_create_provider_with_dimensions(self) -> None:
        from crewlet.config import EmbeddingProviderConfig, create_embedding_provider

        cfg = EmbeddingProviderConfig(
            type="openai",
            model="text-embedding-3-small",
            dimensions=512,
        )
        provider = create_embedding_provider(cfg)
        assert provider.dimensions == 512

    def test_unsupported_type_rejected_at_config_time(self) -> None:
        """``type`` is a closed set, so a typo fails at validation —
        early enough for ``crewlet validate`` and the JSON Schema to
        report it, rather than at provider construction."""
        from pydantic import ValidationError

        from crewlet.config import EmbeddingProviderConfig

        with pytest.raises(ValidationError, match="openai"):
            EmbeddingProviderConfig(type="unsupported")

    def test_unsupported_type_still_raises_in_the_factory(self) -> None:
        """Defense in depth: the factory is callable with a hand-built
        config that bypassed validation, and must not fall through
        silently."""
        from crewlet.config import EmbeddingProviderConfig, create_embedding_provider

        cfg = EmbeddingProviderConfig.model_construct(type="unsupported")
        with pytest.raises(ValueError, match="Unsupported embedding provider type"):
            create_embedding_provider(cfg)
