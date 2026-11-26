# /// script
# requires-python = ">=3.9"
# dependencies = ["redis>=5,<8"]
# ///

"""Read the Rust live room bot pool from Redis shard 0 as JSONL."""

import argparse
import json
import os

import redis


def export_pool(client):
    # The two keys share the same Redis cluster slot.
    with client.pipeline(transaction=True) as pipe:
        pipe.smembers("robot:{0}:idle")
        pipe.hgetall("robot:{0}:inuse")
        idle, leased = pipe.execute()
    for raw in sorted(set(idle) | set(leased), key=int):
        user_id = int(raw)
        if user_id <= 0 or user_id > 2**63 - 1 or raw != str(user_id).encode():
            raise ValueError("invalid robot account ID")
        record = {"user_id": str(user_id), "state": "idle"}
        if raw in leased:
            stamp = int(leased[raw])
            if stamp <= 0 or stamp > 2**63 - 1:
                raise ValueError("invalid robot allocation time")
            record.update(state="inuse", allocated_at=stamp)
        yield record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    url = os.environ.get("REDIS_URL")
    if not url:
        parser.error("set REDIS_URL for Rust shard 0")
    try:
        with redis.Redis.from_url(
            url, socket_timeout=10, protocol=2, lib_name=None, lib_version=None
        ) as client:
            records = list(export_pool(client))
        for record in records:
            print(json.dumps(record, separators=(",", ":")))
    except (redis.RedisError, ValueError) as err:
        parser.exit(1, f"Could not export room bot pool ({type(err).__name__})\n")


if __name__ == "__main__":
    main()
