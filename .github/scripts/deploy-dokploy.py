"""Update the CalDAV service image and verify a new Dokploy deployment."""

import json
import os
import re
import sys
import time
from urllib.error import HTTPError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener

import yaml


class DeployError(Exception):
    pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def dokploy_call(origin, auth_headers, endpoint, *, query=None, body=None):
    url = f"{origin}/api/{endpoint}"
    if query is not None:
        url += "?" + urlencode(query)
    headers = {**auth_headers, "Accept": "application/json"}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    try:
        with build_opener(NoRedirect).open(Request(url, data=data, headers=headers), timeout=20) as response:
            raw = response.read()
            return json.loads(raw) if raw else None
    except HTTPError as exc:
        raise DeployError(f"{endpoint} returned HTTP {exc.code}.") from None
    except (OSError, ValueError):
        raise DeployError(f"{endpoint} request or response failed.") from None


def compose_image(compose, target, image_repository):
    if compose["composeId"] != target or compose["sourceType"] != "raw":
        raise DeployError("Dokploy returned an unexpected Compose target or source type.")
    document = yaml.safe_load(compose["composeFile"])
    service = document["services"]["caldav-mcp"]
    image = service["image"]
    if not isinstance(image, str) or not re.fullmatch(
        re.escape(image_repository) + r"@sha256:[0-9a-f]{64}", image
    ):
        raise DeployError("CalDAV Compose does not use the expected digest-pinned image repository.")
    return document, service, image


def deploy():
    origin = os.environ["DOKPLOY_URL"].rstrip("/")
    token = os.environ["DOKPLOY_API_TOKEN"]
    target = os.environ["DOKPLOY_COMPOSE_ID"]
    repository = os.environ["IMAGE"]
    digest = os.environ["DIGEST"]
    url = urlsplit(origin)
    if url.scheme != "https" or not url.hostname or url.username is not None or url.path or url.query or url.fragment:
        raise DeployError("DOKPLOY_URL must be an HTTPS origin.")
    if not token or not target:
        raise DeployError("Configure the Dokploy deployment credentials and Compose ID.")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise DeployError("The image build did not return a valid digest.")
    access_id = os.environ.get("DOKPLOY_ACCESS_CLIENT_ID")
    access_secret = os.environ.get("DOKPLOY_ACCESS_CLIENT_SECRET")
    if bool(access_id) != bool(access_secret):
        raise DeployError("Configure both Cloudflare Access service token fields.")
    auth_headers = {"x-api-key": token}
    if access_id and access_secret:
        auth_headers["CF-Access-Client-Id"] = access_id
        auth_headers["CF-Access-Client-Secret"] = access_secret
    destination = f"{repository}@{digest}"
    identity = {"composeId": target}

    def call(endpoint, **kwargs):
        return dokploy_call(origin, auth_headers, endpoint, **kwargs)

    def get_compose():
        return compose_image(call("compose.one", query=identity), target, repository)

    def deployments():
        return call("deployment.allByCompose", query=identity)

    version = call("settings.getDokployVersion")
    if version not in ("0.30.8", "v0.30.8"):
        raise DeployError("Dokploy version is outside the verified API contract.")
    document, service, previous = get_compose()
    print(f"Previous image: {previous}", flush=True)
    title = f"GitHub Actions {os.environ['GITHUB_RUN_ID']}/{os.environ['GITHUB_RUN_ATTEMPT']}"
    if any(row["status"] == "running" for row in deployments()):
        raise DeployError("CalDAV already has a running deployment. Inspect Dokploy before retrying.")
    if previous != destination:
        service["image"] = destination
        call("compose.update", body={**identity, "composeFile": yaml.safe_dump(document, sort_keys=False)})
        if get_compose()[2] != destination:
            raise DeployError("Dokploy did not save the published image digest.")

    call("compose.deploy", body={**identity, "title": title})
    print(f"Deployment requested for {destination}.", flush=True)
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        current = [row for row in deployments() if row.get("title") == title]
        if len(current) > 1:
            raise DeployError("Multiple new deployments appeared. Verify the result in Dokploy.")
        if current:
            status = current[0]["status"]
            if status == "done":
                if get_compose()[2] != destination:
                    raise DeployError("The configured image changed during deployment.")
                print(f"Dokploy reports a successful deployment for {destination}.")
                return
            if status in ("error", "cancelled"):
                raise DeployError("Dokploy reported a deployment failure. Inspect the deployment in Dokploy.")
        time.sleep(5)
    raise DeployError("Deployment result is unconfirmed. Check Dokploy before retrying.")


if __name__ == "__main__":
    try:
        deploy()
    except DeployError as exc:
        sys.exit(str(exc))
    except (KeyError, TypeError, ValueError, yaml.YAMLError):
        sys.exit("Dokploy configuration or response format is invalid. Raw data suppressed.")
