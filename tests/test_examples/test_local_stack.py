"""Structural checks on the Plane local test stack.

The Plane stack lives in the main ``docker-compose.yml`` under the
``plane`` profile (one compose file for everything), driven by
``scripts/plane-dev-bootstrap.sh`` — pure config/shell, so CI's honest
checkable floor is YAML structure + ``bash -n`` plus drift guards on the
strings the bootstrap pins.  The ``docker compose config`` and
shellcheck passes run wherever those tools are installed and skip
elsewhere.
"""

from __future__ import annotations

import re
import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[2]
MAIN_COMPOSE = REPO_ROOT / "docker-compose.yml"
BOOTSTRAP = REPO_ROOT / "scripts" / "plane-dev-bootstrap.sh"
NIMBUS_CONFIG = REPO_ROOT / "examples" / "nimbus.config.yaml"

#: The four backend processes sharing the fork's plane-backend image.
BACKEND_SERVICES = {"plane-api", "plane-worker", "plane-beat", "plane-migrator"}

#: service name -> fork image basename (all tagged ${PLANE_APP_RELEASE:-preview}).
APP_IMAGE_BY_SERVICE = {
    "plane-api": "plane-backend",
    "plane-worker": "plane-backend",
    "plane-beat": "plane-backend",
    "plane-migrator": "plane-backend",
    "plane-web": "plane-frontend",
    "plane-space": "plane-space",
    "plane-admin": "plane-admin",
    "plane-live": "plane-live",
    "plane-proxy": "plane-proxy",
}

INFRA_SERVICES = {
    "plane-db",
    "plane-redis",
    "plane-mq",
    "plane-storage",
    "plane-storage-perms",
}

#: The Caddyfile baked into plane-proxy hardcodes these upstream hostnames;
#: each must exist as a network alias on exactly one service.  The S3
#: store (RustFS) carries the ``plane-minio`` alias for the same reason.
ALIAS_BY_SERVICE = {
    "plane-api": "api",
    "plane-web": "web",
    "plane-space": "space",
    "plane-admin": "admin",
    "plane-live": "live",
    "plane-storage": "plane-minio",
}


@pytest.fixture(scope="module")
def main_compose() -> dict[str, Any]:
    # safe_load resolves YAML anchors and ``<<:`` merge keys, so service
    # ``environment`` maps below already include the shared anchors.
    return yaml.safe_load(MAIN_COMPOSE.read_text())


@pytest.fixture(scope="module")
def plane_services(main_compose: dict[str, Any]) -> dict[str, Any]:
    """The services gated behind the ``plane`` profile."""
    return {
        name: svc
        for name, svc in main_compose["services"].items()
        if "plane" in (svc.get("profiles") or [])
    }


@pytest.fixture(scope="module")
def script_text() -> str:
    return BOOTSTRAP.read_text()


class TestPlaneCompose:
    def test_all_fourteen_services_present(self, plane_services: dict) -> None:
        expected = BACKEND_SERVICES | set(APP_IMAGE_BY_SERVICE) | INFRA_SERVICES
        assert set(plane_services) == expected
        assert len(expected) == 14

    def test_every_plane_service_is_profile_gated(
        self, main_compose: dict, plane_services: dict
    ) -> None:
        # The gate must be exact: every plane-prefixed service carries the
        # profile (a plain `docker compose up` leaves the stack out), and
        # nothing outside the prefix sneaks into the profile.
        for name, svc in main_compose["services"].items():
            in_profile = "plane" in (svc.get("profiles") or [])
            assert in_profile == name.startswith("plane-"), name
        assert all(n.startswith("plane-") for n in plane_services)

    def test_fork_images_pinned_to_preview_tag(self, plane_services: dict) -> None:
        for name, basename in APP_IMAGE_BY_SERVICE.items():
            image = plane_services[name]["image"]
            assert image == (
                f"ghcr.io/crewlet/{basename}:${{PLANE_APP_RELEASE:-preview}}"
            ), f"{name} image drifted: {image}"
        # Every ghcr.io/crewlet image in the file carries the pinned tag —
        # nothing floats.
        for name, svc in plane_services.items():
            image = svc["image"]
            if image.startswith("ghcr.io/crewlet/"):
                assert image.endswith(":${PLANE_APP_RELEASE:-preview}"), name

    def test_storage_is_rustfs_behind_the_minio_alias(
        self, plane_services: dict
    ) -> None:
        # MinIO's community image is de-facto deprecated; the store is
        # RustFS but the proxy's baked Caddyfile still expects the
        # plane-minio:9000 upstream, hence the alias (asserted in the
        # alias test) and the endpoint env here.
        storage = plane_services["plane-storage"]
        assert storage["image"].startswith("rustfs/rustfs")
        env = storage["environment"]
        assert env["RUSTFS_ADDRESS"].endswith(":9000")
        for name in BACKEND_SERVICES:
            svc_env = plane_services[name]["environment"]
            assert svc_env["AWS_S3_ENDPOINT_URL"] == "http://plane-minio:9000", name
        # RustFS runs as uid 10001 — the one-shot chown must complete
        # before the server starts on a fresh named volume.
        dep = storage["depends_on"]["plane-storage-perms"]["condition"]
        assert dep == "service_completed_successfully"

    def test_public_url_parameterises_web_url_and_cors(
        self, plane_services: dict
    ) -> None:
        # PLANE_PUBLIC_URL is the remote-host knob: it must feed BOTH
        # WEB_URL (redirect/link origin) and CORS, defaulting to
        # localhost on the published port.
        env = plane_services["plane-api"]["environment"]
        expected = "${PLANE_PUBLIC_URL:-http://localhost:${PLANE_LISTEN_PORT:-8091}}"
        assert env["WEB_URL"] == expected
        assert env["CORS_ALLOWED_ORIGINS"] == expected

    def test_caddyfile_aliases_declared_exactly_once(
        self, plane_services: dict
    ) -> None:
        seen: list[str] = []
        for name, svc in plane_services.items():
            aliases = (svc.get("networks") or {}).get("default", {}).get(
                "aliases"
            ) or []
            seen.extend(aliases)
            expected = ALIAS_BY_SERVICE.get(name)
            assert aliases == ([expected] if expected else []), (
                f"{name} aliases drifted: {aliases}"
            )
        assert sorted(seen) == sorted(ALIAS_BY_SERVICE.values())

    def test_mq_permits_celerys_transient_queues(
        self, main_compose: dict, plane_services: dict
    ) -> None:
        # RabbitMQ 4.x FORBIDS the deprecated transient non-exclusive
        # queues Celery's control plane declares — without this permit
        # the plane worker crash-loops on Queue.declare 541 and webhook
        # deliveries (Celery tasks) never run.
        permit = main_compose["configs"]["plane-mq-permits"]["content"]
        assert "deprecated_features.permit.transient_nonexcl_queues = true" in permit
        mq_configs = plane_services["plane-mq"]["configs"]
        assert any(
            c["source"] == "plane-mq-permits"
            and c["target"].startswith("/etc/rabbitmq/conf.d/")
            for c in mq_configs
        )

    def test_webhook_private_urls_in_anchor_and_inherited(
        self, main_compose: dict, plane_services: dict
    ) -> None:
        # The fork's private-address webhook permit: the setting is
        # env-read at webhook create/update (api) AND delivery (worker
        # Celery task) — the shared anchor must carry it and both
        # services must inherit it.
        assert main_compose["x-plane-app-env"]["WEBHOOK_ALLOW_PRIVATE_URLS"] == "1"
        for name in ("plane-api", "plane-worker"):
            env = plane_services[name]["environment"]
            assert env["WEBHOOK_ALLOW_PRIVATE_URLS"] == "1", name

    def test_api_and_worker_reach_the_host(self, plane_services: dict) -> None:
        # Webhook validation happens in the api, delivery in the worker —
        # both need the host mapping to reach host.docker.internal:8000.
        for name in ("plane-api", "plane-worker"):
            extra = plane_services[name].get("extra_hosts") or []
            assert "host.docker.internal:host-gateway" in extra, name

    def test_plane_volumes_declared_and_mounted(
        self, main_compose: dict, plane_services: dict
    ) -> None:
        declared_plane = {v for v in main_compose["volumes"] if v.startswith("plane-")}
        mounted: set[str] = set()
        for svc in plane_services.values():
            for mount in svc.get("volumes") or []:
                source = mount.split(":", 1)[0]
                if not source.startswith(("/", ".")):
                    mounted.add(source)
        # Every named volume a plane service mounts is declared with the
        # plane- prefix, and every declared plane- volume is mounted
        # somewhere (no orphans either way).  Non-plane services must not
        # mount plane- volumes.
        assert mounted == declared_plane
        for name, svc in main_compose["services"].items():
            if name in plane_services:
                continue
            for mount in svc.get("volumes") or []:
                assert not mount.split(":", 1)[0].startswith("plane-"), name

    def test_published_ports_single_proxy_surface(
        self, main_compose: dict, plane_services: dict
    ) -> None:
        def host_ports(services: dict) -> set[str]:
            ports: set[str] = set()
            for svc in services.values():
                for entry in svc.get("ports") or []:
                    # "host:container" or "ip:host:container" strings; the
                    # host part may be a "${VAR:-default}" interpolation
                    # (which itself contains a colon).
                    head = str(entry).rsplit(":", 1)[0]
                    if ":" in head and not head.startswith("${"):
                        head = head.rsplit(":", 1)[-1]
                    ports.add(head)
            return ports

        plane_ports = host_ports(plane_services)
        # The proxy is the stack's single published surface.
        assert plane_ports == {"${PLANE_LISTEN_PORT:-8091}"}
        proxy_ports = plane_services["plane-proxy"]["ports"]
        assert proxy_ports == ["${PLANE_LISTEN_PORT:-8091}:80"]
        # 8091 (the default) is free in the rest of the dev story.
        rest = {
            name: svc
            for name, svc in main_compose["services"].items()
            if name not in plane_services
        }
        assert "8091" not in host_ports(rest)

    def test_restart_policies(self, plane_services: dict) -> None:
        # Two one-shots: the migrator retries a FAILED migration
        # (on-failure, upstream CE's policy) but never restarts a clean
        # exit; the storage chown runs once. Everything else stays up.
        expected_by_name = {
            "plane-migrator": "on-failure:3",
            "plane-storage-perms": "no",
        }
        for name, svc in plane_services.items():
            expected = expected_by_name.get(name, "unless-stopped")
            assert str(svc["restart"]) == expected, name

    def test_depends_on_healthy_where_healthchecks_exist(
        self, plane_services: dict
    ) -> None:
        # The healthchecked services (upstream ships none; repo
        # convention adds them where a dependency can gate on them).
        assert "healthcheck" in plane_services["plane-db"]
        assert "healthcheck" in plane_services["plane-storage"]
        api_check = plane_services["plane-api"]["healthcheck"]
        assert "/api/instances/" in " ".join(api_check["test"])
        # First boot waits on the one-shot migrator's FULL migration set
        # AND collectstatic before serving — the budget must cover a cold
        # boot (≈10 min), because five dependents gate on service_healthy
        # and `up -d` aborts them when the budget blows.
        assert api_check["start_period"] == "300s"
        assert api_check["retries"] == 30
        api_deps = plane_services["plane-api"]["depends_on"]
        assert api_deps["plane-db"]["condition"] == "service_healthy"
        # The api entrypoint's create_bucket needs the store ANSWERING.
        assert api_deps["plane-storage"]["condition"] == "service_healthy"
        assert (
            plane_services["plane-proxy"]["depends_on"]["plane-api"]["condition"]
            == "service_healthy"
        )
        assert (
            plane_services["plane-worker"]["depends_on"]["plane-api"]["condition"]
            == "service_healthy"
        )

    @pytest.mark.skipif(shutil.which("docker") is None, reason="docker not installed")
    def test_docker_compose_config_validates(self) -> None:
        probe = subprocess.run(
            ["docker", "compose", "version"], capture_output=True, cwd=REPO_ROOT
        )
        if probe.returncode != 0:
            pytest.skip("docker compose plugin not available")
        for args in (
            ["--profile", "plane"],
            [],  # base profile must stay valid (and plane-free) on its own
        ):
            result = subprocess.run(
                ["docker", "compose", *args, "config", "-q"],
                capture_output=True,
                text=True,
                cwd=REPO_ROOT,
            )
            assert result.returncode == 0, result.stderr


class TestPlaneBootstrapScript:
    def test_bash_syntax(self) -> None:
        result = subprocess.run(
            ["bash", "-n", str(BOOTSTRAP)], capture_output=True, text=True
        )
        assert result.returncode == 0, result.stderr

    def test_script_is_executable(self) -> None:
        assert BOOTSTRAP.stat().st_mode & 0o111

    def test_pinned_strings(self, script_text: str) -> None:
        # Drift guards on the strings the rest of the walkthrough relies
        # on: the webhook route, the workspace slug, the provisioner
        # invocation, the env file, the single import walk, the
        # django-shell founder pass, the cache flush, the profile-gated
        # compose invocation, the PLANE_URL persist, and the demo-project
        # archive step.
        for pin in (
            "/webhooks/plane",
            'WORKSPACE_SLUG="nimbus"',
            "crewlet plane provision",
            "--create-projects",
            "--webhook-url",
            ".env.plane",
            "PLANE_FOUNDER_USER_ID",
            'crewlet plane import "${COMPANY}" examples/',
            "manage.py shell",
            "manage.py clear_cache",
            "docker compose --profile plane",
            'upsert_env_var "${ENV_FILE}" PLANE_URL',
            "Welcome to the Plane Demo Project",
            "/archive/",
        ):
            assert pin in script_text, f"bootstrap lost its pin: {pin}"
        # The top-level `crewlet api` command does not exist.
        assert "crewlet api " not in script_text

    def test_public_url_chain(self, script_text: str) -> None:
        # The remote-host knob: explicit PLANE_URL > PLANE_PUBLIC_URL
        # (the compose knob) > localhost on the published port.
        assert (
            'PLANE_URL="${PLANE_URL:-${PLANE_PUBLIC_URL:-'
            'http://localhost:${PLANE_LISTEN_PORT:-8091}}}"' in script_text
        )

    def test_single_host_walkthrough_is_embedded_api_only(
        self, script_text: str
    ) -> None:
        """The embedded-API-port topology invariant.

        Single-host walkthroughs use the engine's EMBEDDED API as the
        webhook target: the example Tier A file sets ``api.port: 80``
        (so ``crewlet run`` binds it and serves ``/webhooks/plane``;
        binding 80 needs privileged-port access on Linux — documented in
        the config), and the bootstrap's next-steps print must therefore
        contain exactly one ``crewlet run`` command per branch and NEVER
        the standalone ``crewlet run api`` — a second binder on the same
        port kills the engine's embedded server with EADDRINUSE →
        SystemExit.
        """
        config = yaml.safe_load(NIMBUS_CONFIG.read_text())
        assert config["api"]["port"] == 80
        # The registered webhook target must carry the same port.
        script = BOOTSTRAP.read_text()
        assert "http://host.docker.internal:80/webhooks/plane" in script
        # Comments may (and do) explain the prohibition; only non-comment
        # lines count — same convention as the --wait guard above.
        code_text = "\n".join(
            line
            for line in script_text.splitlines()
            if not line.lstrip().startswith("#")
        )
        assert "crewlet run api" not in code_text
        # One engine-run command per next-steps branch (COMPANY set/unset).
        run_commands = re.findall(r"crewlet run (\S+)", code_text)
        assert run_commands == [
            "examples/nimbus.config.yaml",
            "examples/nimbus.config.yaml",
        ]

    def test_no_compose_wait_flag(self, script_text: str) -> None:
        # `up -d --wait` treats the exited one-shot migrator as a
        # failure — the script must poll /api/instances/ instead. Comments
        # may (and do) explain the prohibition, so only code lines count.
        code_lines = [
            line
            for line in script_text.splitlines()
            if not line.lstrip().startswith("#")
        ]
        assert not any("--wait" in line for line in code_lines)
        assert "/api/instances/" in script_text

    @pytest.mark.skipif(
        shutil.which("shellcheck") is None, reason="shellcheck not installed"
    )
    def test_shellcheck(self) -> None:
        result = subprocess.run(
            ["shellcheck", str(BOOTSTRAP)], capture_output=True, text=True
        )
        assert result.returncode == 0, result.stdout
