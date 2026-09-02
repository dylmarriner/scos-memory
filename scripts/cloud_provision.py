#!/usr/bin/env python3
"""Exchange a one-time provisioning token for real SCOS credentials, write
them to ~/.hermes/scos.env, then link every local agent bundle -- the CLI
half of the dashboard's "Connect this machine" flow (POST
/api/cli/provisioning-token mints the token; this script consumes it via
POST /api/cli/exchange).

Must be run from within a checkout of this repository (like
install_agent_plugins.py, which it calls directly once scos.env is
written) -- it does not fetch the repo itself. See install.sh for the
thin bootstrap a fresh machine actually runs.
"""
from __future__ import annotations

import argparse
import json
import shutil
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import install_agent_plugins  # noqa: E402  (path must be set up first)

SCOS_ENV = Path.home() / ".hermes" / "scos.env"
STAMP = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def exchange(base_url: str, token: str) -> dict:
    req = urllib.request.Request(
        base_url.rstrip("/") + "/api/cli/exchange",
        data=json.dumps({"token": token}).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        raise SystemExit(f"exchange failed: HTTP {e.code}: {body[:300]}")
    except urllib.error.URLError as e:
        raise SystemExit(f"could not reach {base_url}: {e}")


def write_scos_env(creds: dict) -> None:
    if SCOS_ENV.exists():
        backup = SCOS_ENV.with_name(SCOS_ENV.name + f".bak-{STAMP}")
        shutil.copy2(SCOS_ENV, backup)
        print(f"backed up existing {SCOS_ENV} -> {backup}")
    SCOS_ENV.parent.mkdir(parents=True, exist_ok=True)
    SCOS_ENV.write_text(
        "SCOS_GATEWAY_URL={gateway_url}\n"
        "SCOS_BEARER_TOKEN={bearer_token}\n"
        "SCOS_TENANT_ID={tenant_id}\n"
        "SCOS_PRINCIPAL_ID={principal_id}\n".format(**creds)
    )
    print(f"wrote {SCOS_ENV}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--token", required=True, help="one-time provisioning token from the dashboard")
    parser.add_argument("--base-url", required=True, help="cloud-control-plane origin, e.g. https://scos.example.com")
    parser.add_argument("--skip-gemini", action="store_true")
    args = parser.parse_args()

    creds = exchange(args.base_url, args.token)
    for key in ("gateway_url", "bearer_token", "tenant_id", "principal_id"):
        if not creds.get(key):
            raise SystemExit(f"exchange response missing {key}: {creds}")

    write_scos_env(creds)
    install_agent_plugins.install(skip_gemini=args.skip_gemini)
    print("\nDone -- every local agent bundle above is now linked to your SCOS account.")


if __name__ == "__main__":
    main()
