#!/usr/bin/env python3
"""Run a set: for each SCENARIO:N, create a fresh sandbox through
Differently, run cycle.py on it N times, delete it.
usage: runset.py PREFIX idle:3 in-use:5
PAUSE_DROP_CHECKPOINTS=1 also clears consumed checkpoints from the node's
image cache after each sandbox is deleted (test hosts with a small disk)."""
import os, subprocess, sys, time
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import dly
prefix = sys.argv[1]
for spec in sys.argv[2:]:
    scenario, n = spec.split(":")
    t0 = time.time()
    code, s = dly.create()
    s = dly.wait(s["id"], "running", timeout=900)
    print(f"[{prefix}-{scenario}] created {s['id']} in {time.time()-t0:.1f} s", flush=True)
    time.sleep(20)
    subprocess.run(["python3", os.path.join(HERE, "cycle.py"), s["id"], scenario, n, f"{prefix}-{scenario}"])
    dly.call("DELETE", "/v1/sandboxes/" + s["id"])
    time.sleep(15)
    if os.environ.get("PAUSE_DROP_CHECKPOINTS") == "1":
        subprocess.run(["sudo", "sh", os.path.join(HERE, "drop-checkpoints.sh")])
print(f"[{prefix}] done", flush=True)
