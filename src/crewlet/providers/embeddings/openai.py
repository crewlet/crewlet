"""OpenAI embedding provider using the official openai SDK."""

from __future__ import annotations

from openai import AsyncOpenAI

from crewlet._logging import get_logger
from crewlet.secrets.resolver import resolve_env

logger = get_logger("embeddings.openai")


class OpenAIEmbeddingProvider:
    """OpenAI-compatible embedding provider using the official SDK.

    Works with OpenAI API and any compatible endpoint by setting
    ``base_url``.
    """

    def __init__(
        self,
        model: str = "text-embedding-3-small",
        api_key: str = "",
        base_url: str = "https://api.openai.com/v1",
        dimensions: int = 1536,
    ) -> None:
        self.model = model
        self.api_key = api_key or resolve_env("OPENAI_API_KEY")
        self.base_url = base_url.rstrip("/")
        self._dimensions = dimensions
        # Use a placeholder key during construction to avoid SDK validation
        # errors. The real key is only needed at request time.
        self._client = AsyncOpenAI(
            api_key=self.api_key or "not-set",
            base_url=self.base_url,
            timeout=60.0,
        )
        logger.debug(
            "provider_created",
            model=self.model,
            dimensions=self._dimensions,
        )

    @property
    def dimensions(self) -> int:
        return self._dimensions

    async def embed(self, texts: list[str]) -> list[list[float]]:
        """Generate embeddings for a list of texts."""
        if not texts:
            return []

        logger.debug(
            "embed_request",
            model=self.model,
            text_count=len(texts),
        )

        response = await self._client.embeddings.create(
            model=self.model,
            input=texts,
        )

        # Sort by index to ensure correct ordering
        sorted_data = sorted(response.data, key=lambda x: x.index)
        embeddings = [item.embedding for item in sorted_data]

        usage = response.usage
        logger.info(
            "embed_complete",
            model=self.model,
            text_count=len(texts),
            total_tokens=usage.total_tokens if usage else 0,
        )

        return embeddings

    async def close(self) -> None:
        """Close the underlying HTTP client."""
        await self._client.close()
