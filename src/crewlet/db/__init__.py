"""Database layer — asyncpg client, migrator, storage protocol, and repositories."""

from crewlet.db.client import Database, DatabaseConnection, SQLExecutor
from crewlet.db.company_config import CompanyConfigRevision, CompanyConfigStore
from crewlet.db.leases import (
    Lease,
    LeaseBackend,
    LeaseStore,
    MemoryLeaseStore,
    seat_resource,
    worker_resource,
)
from crewlet.db.migrator import migrate
from crewlet.db.protocol import StorageBackend
from crewlet.db.secret_values import (
    DatabaseSecretSource,
    SecretStoreError,
    SecretValueRecord,
    SecretValueStore,
    load_secret_source,
)
from crewlet.db.token_usage import TokenUsageRepository

__all__ = [
    "CompanyConfigRevision",
    "CompanyConfigStore",
    "Database",
    "DatabaseConnection",
    "DatabaseSecretSource",
    "Lease",
    "LeaseBackend",
    "LeaseStore",
    "MemoryLeaseStore",
    "SQLExecutor",
    "SecretStoreError",
    "SecretValueRecord",
    "SecretValueStore",
    "StorageBackend",
    "TokenUsageRepository",
    "load_secret_source",
    "migrate",
    "seat_resource",
    "worker_resource",
]
