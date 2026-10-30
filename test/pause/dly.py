"""A small Differently API client for the pause runs (standard library only).
env: DIFFERENTLY_URL (default http://localhost:8080), DIFFERENTLY_API_KEY
(a project key), DIFFERENTLY_POOL (default opensandbox-microvm)."""
import json, os, time, urllib.error, urllib.request

BASE = os.environ.get("DIFFERENTLY_URL", "http://localhost:8080").rstrip("/")
POOL = os.environ.get("DIFFERENTLY_POOL", "opensandbox-microvm")


def call(method, path, body=None, timeout=1800):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method, headers={
        "Authorization": "Bearer " + os.environ["DIFFERENTLY_API_KEY"], "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, raw.decode(errors="replace")


def create(ttl=7200):
    return call("POST", "/v1/sandboxes", {"placement": {"allowedPools": [POOL], "external": "never"},
                                          "ttlSeconds": ttl, "idleTimeoutSeconds": -1, "labels": {"purpose": "pause-runs"}})


def wait(sid, status, timeout=1800):
    t0 = time.time()
    while time.time() - t0 < timeout:
        code, s = call("GET", "/v1/sandboxes/" + sid)
        if code == 200 and s.get("status") == status:
            return s
        if code == 200 and s.get("status") in ("failed", "terminated"):
            raise SystemExit(f"{sid} is {s.get('status')}: {s.get('statusReason')}")
        time.sleep(0.25)
    raise SystemExit(f"{sid}: timed out waiting for {status}")


def exe(sid, cmd, timeout=600):
    code, r = call("POST", f"/v1/sandboxes/{sid}/exec", {"command": cmd, "timeoutSeconds": timeout}, timeout=timeout + 30)
    if code != 200:
        raise SystemExit(f"exec {code}: {r}")
    return r
