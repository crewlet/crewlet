"""Embedding provider abstractions."""

from crewlet.providers.embeddings.protocol import EmbeddingProvider

__all__ = ["EmbeddingProvider", "OpenAIEmbeddingProvider"]


def __getattr__(name: str) -> object:
    if name == "OpenAIEmbeddingProvider":
        from crewlet.providers.embeddings.openai import OpenAIEmbeddingProvider

        return OpenAIEmbeddingProvider
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
