#!/usr/bin/env python3
"""Seed the isolated AgentDeck knowledge demo with curated entries.

The target must be the demo instance (default 127.0.0.1:8327), authenticated
with its gateway token. The seed file is a JSON array of knowledge_add
payloads; the server still runs its own dedupe/sensitive checks.

Usage:
    AGENTDECK_TOKEN=... python3 scripts/seed-knowledge-demo.py
    AGENTDECK_TOKEN=... python3 scripts/seed-knowledge-demo.py \
        --file docs/knowledge-seed-harmony-build-machines.json \
        --url http://127.0.0.1:8327

The token is never printed. Re-running is idempotent for identical
project+title+body (the server returns duplicate=true and the existing id).
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


def post_json(url, token, payload):
    query = urllib.parse.urlencode({"token": token})
    req = urllib.request.Request(
        url + "/api/knowledge?" + query,
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        body = err.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(body)
        except Exception:
            parsed = {"error": body}
        return err.code, parsed
    except urllib.error.URLError as err:
        return 0, {"error": str(err)}


def main():
    parser = argparse.ArgumentParser(description="Seed AgentDeck knowledge demo")
    parser.add_argument("--url", default="http://127.0.0.1:8327")
    parser.add_argument("--file", default="docs/knowledge-seed-harmony-build-machines.json")
    parser.add_argument("--token", default=os.environ.get("AGENTDECK_TOKEN", ""))
    args = parser.parse_args()
    if not args.token:
        print("missing AGENTDECK_TOKEN (or --token)", file=sys.stderr)
        return 2
    try:
        with open(args.file, encoding="utf-8") as fh:
            entries = json.load(fh)
    except (OSError, json.JSONDecodeError) as err:
        print("read seed file: %s" % err, file=sys.stderr)
        return 2
    if not isinstance(entries, list):
        print("seed file must be a JSON array", file=sys.stderr)
        return 2

    base = args.url.rstrip("/")
    created = duplicates = failed = 0
    for entry in entries:
        title = str(entry.get("title", "")).strip() or "(untitled)"
        status, result = post_json(base, args.token, entry)
        if status in (200, 201) and result.get("ok"):
            entry_id = result.get("id") or (result.get("entry") or {}).get("id", "")
            dup = bool(result.get("duplicate"))
            if dup:
                duplicates += 1
            else:
                created += 1
            print("%s  %s  %s  %s" % ("dup" if dup else "new", entry_id,
                                      (result.get("status") or (result.get("entry") or {}).get("status", "")),
                                      title))
        else:
            failed += 1
            print("FAIL  %s  HTTP %s  %s" % (title, status, result.get("error", "")))
    print("\ncreated=%d duplicate=%d failed=%d" % (created, duplicates, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
