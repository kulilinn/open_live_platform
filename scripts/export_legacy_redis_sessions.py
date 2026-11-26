# /// script
# requires-python = ">=3.9"
# dependencies = ["redis>=5,<8"]
# ///

"""Export Rust AuthUser Redis records from one Redis database or source shard."""

import argparse
import base64
from datetime import datetime, timedelta, timezone
import json
import os

import redis


# Read the raw value, expiry and Redis's clock in a single atomic operation.
READ_SESSION = """
local value = redis.call('GET', KEYS[1])
if not value then return {} end
local clock = redis.call('TIME')
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then return {} end
return {value, clock[1], clock[2], ttl}
"""


def export_sessions(client, app_name):
    suffix = ("|auth_user|" + app_name).encode()
    read_session = client.register_script(READ_SESSION)
    for key in client.scan_iter(match="*|auth_user|*"):
        if not key.endswith(suffix):
            continue
        result = read_session(keys=[key])
        if not result:
            continue
        value, seconds, microseconds, ttl = result
        captured_at = datetime.fromtimestamp(int(seconds), timezone.utc)
        expires_at = captured_at + timedelta(milliseconds=int(microseconds) // 1000 + ttl)
        yield {
            "key": key.decode(),
            "value_base64": base64.b64encode(value).decode(),
            "expires_at": expires_at.isoformat(timespec="milliseconds"),
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", required=True, help="the original Rust APP_NAME")
    args = parser.parse_args()
    if not args.app or not os.environ.get("REDIS_URL"):
        parser.error("REDIS_URL and a nonempty --app are required")
    with redis.Redis.from_url(os.environ["REDIS_URL"], socket_timeout=10) as client:
        for record in export_sessions(client, args.app):
            print(json.dumps(record, separators=(",", ":")))


if __name__ == "__main__":
    main()
