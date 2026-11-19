#!/usr/bin/env python3
"""Reproduce: the in-container Caveman proxy forwards chat completions
byte-for-byte (no compression) at a large latency cost.

Run this INSIDE the Plurality container (loopback-only endpoints):

    docker exec Simplify python3 /tmp/caveman_proxy_repro.py

It builds ONE payload containing a single dense `role: tool` result (Caveman's
documented best case: JSON), sends it through both paths, and prints the
provider-reported prompt_tokens for each. If the proxy compresses, the
CAVEMAN line shows a clearly smaller prompt_tokens than the LITELLM line.

Observed on master 8ac8067, instance built 2026-11-18:

    payload bytes: 469676  tool msg chars: 399692
    CAVEMAN  prompt_tokens=196842 completion=1 in 99.8s   (1st attempt timed out at 180s)
    LITELLM  prompt_tokens=196842 completion=1 in  0.2s

Identical prompt_tokens => zero tokens saved; ~500x the latency.

Cross-check in the proxy's own ledger (never writes a compression plan):

    sqlite3 /app/multiply-data/caveman/caveman.db \\
      "select (select count(*) from middleware_plans),
              (select count(*) from middleware_originals),
              (select count(*) from middleware_receipts),
              (select count(*) from requests);"
    -> 0|0|0|0

Why (per https://docs.caveman.so/caveman/middleware): compression requires a
recovery tool the client registered itself, plus a per-request scope. An entry
point that cannot offer recovery only records: in `compress` mode it forwards
content unchanged and reports `recovery_unbound`. Plurality routes the proxy as
a transparent forwarder and registers neither, so this is the specified
behaviour, not a crash.
"""

import json
import time
import urllib.request

CAVEMAN = "http://127.0.0.1:8789/compat/litellm/v1/chat/completions"
LITELLM = "http://127.0.0.1:4000/v1/chat/completions"

# Model must be one that opts into caveman in litellm_config.yaml
# (model_info.caveman: true) for the proxy to be in its path at all.
MODEL = "strata-swift"

N_RECORDS = 1200


def build_payload():
    """A realistic oversized tool result: dense JSON array of records.

    Returns (body_bytes, tool_content_chars).
    """
    big = json.dumps(
        [
            {
                "name": f"container{i}",
                "image": "nginx:1.2x",
                "status": "running",
                "state": "running",
                "ports": [
                    {"ip": "172.17.0.3", "privatePort": 80, "publicPort": 8080 + i}
                ],
                "labels": {"com.example/owner": "team"},
                "created": "2026-11-01T00:00:00Z",
                "id": "%012x" % i,
            }
            for i in range(N_RECORDS)
        ],
        indent=2,
    )
    messages = [
        {"role": "system", "content": "You are terse."},
        {"role": "user", "content": "say ok"},
        {
            "role": "assistant",
            "content": "checking",
            "tool_calls": [
                {
                    "id": "t1",
                    "type": "function",
                    "function": {
                        "name": "cosmos__cosmos_docker_list",
                        "arguments": "{}",
                    },
                }
            ],
        },
        {"role": "tool", "tool_call_id": "t1", "content": big},
    ]
    body = json.dumps(
        {"model": MODEL, "messages": messages, "max_tokens": 1, "stream": False}
    )
    return body.encode(), len(big)


def post(url: str, body: bytes, timeout: int = 180):
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"}
    )
    t = time.time()
    r = json.load(urllib.request.urlopen(req, timeout=timeout))
    return r.get("usage", {}), time.time() - t


def main() -> None:
    body, tool_chars = build_payload()
    print(f"payload bytes: {len(body)}  tool msg chars: {tool_chars}", flush=True)

    for label, url in (("CAVEMAN", CAVEMAN), ("LITELLM", LITELLM)):
        for attempt in (0, 1):  # retry once: the proxy is intermittently slow
            try:
                usage, secs = post(url, body)
                print(
                    f"{label:8} prompt_tokens={usage.get('prompt_tokens')} "
                    f"completion={usage.get('completion_tokens')} in {secs:.1f}s",
                    flush=True,
                )
                break
            except Exception as e:  # noqa: BLE001 - repro script
                print(
                    f"{label:8} attempt{attempt} ERROR {type(e).__name__}: {e}",
                    flush=True,
                )


if __name__ == "__main__":
    main()
