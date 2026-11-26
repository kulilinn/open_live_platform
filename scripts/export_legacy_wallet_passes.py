# /// script
# requires-python = ">=3.9"
# dependencies = ["redis>=5,<8"]
# ///

"""Export active Rust wallet-pass download keys from one Redis shard."""

import argparse
from datetime import datetime, timedelta, timezone
import json
import os

import redis


READ_PASS = """
local object = redis.call('GET', KEYS[1])
if not object then return {} end
local clock = redis.call('TIME')
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then return {} end
return {object, clock[1], clock[2], ttl}
"""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", default=os.getenv("APP_NAME", "unknown"))
    args = parser.parse_args()
    if not os.environ.get("REDIS_URL"):
        parser.error("REDIS_URL is required")
    suffix = f"|pkpass|{args.app}".encode()
    with redis.Redis.from_url(os.environ["REDIS_URL"], socket_timeout=10) as client:
        read_pass = client.register_script(READ_PASS)
        for key in client.scan_iter(match="*|pkpass|*"):
            if not key.endswith(suffix):
                continue
            result = read_pass(keys=[key])
            if not result:
                continue
            object_key, seconds, microseconds, ttl = result
            timestamp = datetime.fromtimestamp(int(seconds), timezone.utc)
            expires_at = timestamp + timedelta(milliseconds=int(microseconds) // 1000 + ttl)
            print(json.dumps({"key": key.decode(), "object": object_key.decode(),
                              "expires_at": expires_at.isoformat(timespec="milliseconds")}))


if __name__ == "__main__":
    main()
