"""Deploy a published main-branch image using the Shanghai host's infra CLI."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
from urllib.request import Request, urlopen

GITHUB = "https://api.github.com/repos/Seeridia/caldav-mcp-go"
ENVIRONMENT = "caldav-production-image"
REPOSITORY = "hkccr.ccs.tencentyun.com/seeridia-static/caldav-mcp"
COMPOSE = "rCHsaT3hOWwHjgX5-EyhY"
SERVICE = "caldav-mcp"


def github(path):
    request = Request(GITHUB + path, headers={"Accept": "application/vnd.github+json"})
    with urlopen(request, timeout=15) as response:
        return json.load(response)


def main_head():
    return github("/branches/main")["commit"]["sha"]


def latest_image(head):
    rows = github(f"/deployments?environment={ENVIRONMENT}&sha={head}&per_page=10")
    for row in rows:
        if row["sha"] != head or row["environment"] != ENVIRONMENT:
            continue
        creator = row.get("creator") or {}
        if creator.get("login") != "github-actions[bot]" or creator.get("type") != "Bot":
            continue
        payload = row["payload"]
        if isinstance(payload, str):
            payload = json.loads(payload)
        digest = payload.get("digest", "")
        if re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            return f"{REPOSITORY}@{digest}"
    return None


def infra(*args):
    result = subprocess.run([os.environ["INFRA_CLI"], *args], capture_output=True, timeout=120)
    if result.returncode:
        raise RuntimeError(f"infra {args[0]} failed; inspect the host-side infra receipt.")
    return json.loads(result.stdout)


def configured_image():
    compose, = infra("inspect", "composes", "--id", COMPOSE)["data"]
    if compose["composeId"] != COMPOSE or compose["sourceType"] != "raw":
        raise RuntimeError("Unexpected production Compose identity or source type.")
    image = compose["composeFile_summary"]["services"][SERVICE]["image"]
    if not re.fullmatch(re.escape(REPOSITORY) + r"@sha256:[0-9a-f]{64}", image):
        raise RuntimeError("Production Compose does not use the expected image repository and digest.")
    return image


def runs():
    return infra("runs", "--owner", COMPOSE)["data"]


def apply(operation, *args):
    plan = infra("plan", operation, "--id", COMPOSE, *args)["plan"]["plan_id"]
    infra("apply", plan)


def save(path, state):
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as stream:
        json.dump(state, stream)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(stream.name, path)


def reconcile(path):
    state = json.loads(path.read_text()) if path.exists() else {}
    if state and state["status"] != "done":
        raise RuntimeError("Previous deployment requires reconciliation in Dokploy before retrying.")
    sha = main_head()
    if state.get("sha") == sha:
        return
    image = latest_image(sha)
    if image is None:
        return
    previous = configured_image()
    if previous == image and state.get("image") == image:
        save(path, {"status": "done", "sha": sha, "image": image})
        return
    before = runs()
    if any(row["status"] == "running" for row in before):
        raise RuntimeError("A deployment is already running for the production Compose.")
    existing = {row["deploymentId"] for row in before}
    if main_head() != sha:
        return
    save(path, {"status": "started", "sha": sha, "previous": previous, "image": image})
    print(f"Deploying {sha}: {previous} -> {image}", flush=True)
    if previous != image:
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", dir=path.parent) as spec:
            json.dump({"images": {SERVICE: image}}, spec)
            spec.flush()
            apply("compose-images", "--spec", spec.name)
        if configured_image() != image:
            raise RuntimeError("Dokploy did not save the published image digest.")
    apply("compose-deploy")
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        current = [row for row in runs() if row["deploymentId"] not in existing]
        if len(current) > 1:
            raise RuntimeError("Concurrent deployment records appeared. Verify the result in Dokploy.")
        if current:
            if current[0]["status"] in ("error", "cancelled"):
                raise RuntimeError("Dokploy reported an unsuccessful deployment.")
            if current[0]["status"] == "done":
                if configured_image() != image:
                    raise RuntimeError("The configured image changed during deployment.")
                save(path, {"status": "done", "sha": sha, "image": image})
                print(f"Dokploy reports a successful deployment for {image}.", flush=True)
                return
        time.sleep(5)
    raise RuntimeError("The deployment result is unconfirmed. Check Dokploy before retrying.")


if __name__ == "__main__":
    try:
        reconcile(Path(os.environ["STATE_DIRECTORY"]) / "deployment.json")
    except RuntimeError as error:
        sys.exit(str(error))
    except (OSError, ValueError, TypeError, KeyError, AttributeError, subprocess.TimeoutExpired):
        sys.exit("Deployment check failed. Inspect GitHub availability and the local infra receipts.")
