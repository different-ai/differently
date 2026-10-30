#!/usr/bin/env python3
"""Pause and resume one sandbox N times through Differently, timing each
phase from the Fast Sandbox logs and checking the guest kept its state.
Run on the KVM node (it reads k3s logs and the disk with sudo).
usage: cycle.py SANDBOX_ID idle|in-use N TAG   -> $PAUSE_RUNS/TAG.jsonl"""
import datetime as dt, json, os, subprocess, sys, time
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import dly

sid, scenario, n, tag = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
RUNS = os.environ.get("PAUSE_RUNS", os.path.join(HERE, "runs"))
os.makedirs(RUNS, exist_ok=True)
out = open(os.path.join(RUNS, f"{tag}.jsonl"), "a", buffering=1)
samples = os.path.join(RUNS, f"{tag}.samples.jsonl")
sampler = subprocess.Popen(["sudo", "python3", os.path.join(HERE, "sampler.py"), samples, os.environ.get("STATE_ROOT_DEVICE", "nvme0n1")])
GUEST_USE = open(os.path.join(HERE, "guest-use.sh")).read()
GUEST_STATE = open(os.path.join(HERE, "guest-state.sh")).read()

def guest(script, *args):
    r = dly.exe(sid, "cat > /tmp/g.sh <<'GEOF'\n" + script + "\nGEOF\nsh /tmp/g.sh " + " ".join(args), timeout=900)
    return r.get("stdout", "")

def state():
    lines = guest(GUEST_STATE).splitlines()
    state = {l.split()[0]: " ".join(l.split()[1:]) for l in lines if not l.startswith(("proc ", "fd "))}
    state["_fds"] = [l[3:] for l in lines if l.startswith("fd ")]
    return state, [l for l in lines if l.startswith("proc ")]

def fsb_name():
    out = subprocess.run(["sudo", "k3s", "kubectl", "-n", os.environ.get("FSB_DATAPLANE_NAMESPACE", "opensandbox-dataplane"), "get", "sandboxes", "-o", "json"], capture_output=True, text=True).stdout
    for item in json.loads(out)["items"]:
        if sid in json.dumps(item.get("metadata", {})) or sid in json.dumps(item.get("spec", {}).get("metadata", {})):
            return item["metadata"]["name"]
    return None

def garage_bytes():
    return int(subprocess.run(["sudo", "du", "-sb", "/var/lib/fast-sandbox/garage/blocks"], capture_output=True, text=True).stdout.split()[0])

try:
    if scenario == "in-use":
        print(guest(GUEST_USE, "setup"), file=sys.stderr)
    for i in range(1, n + 1):
        rec = {"tag": tag, "scenario": scenario, "cycle": i, "sandbox": sid}
        if scenario == "in-use" and i > 1:
            rec["use"] = guest(GUEST_USE, "step", str(i)).strip()
        before, procs_before = state()
        name = fsb_name(); rec["fsb"] = name
        g0 = garage_bytes()
        since = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        time.sleep(1)
        t0 = time.time()
        code, body = dly.call("POST", f"/v1/sandboxes/{sid}/pause", {})
        rec["pause_post"] = {"code": code, "s": round(time.time() - t0, 2)}
        dly.wait(sid, "paused", timeout=1800)
        rec["pause_s"] = round(time.time() - t0, 2)
        time.sleep(3)
        rec["garage_stored_mb"] = round((garage_bytes() - g0) / 1e6)
        ph = subprocess.run(["sudo", "python3", os.path.join(HERE, "phases.py"), since, name, samples], capture_output=True, text=True)
        try:
            rec["node"] = json.loads(ph.stdout)
        except ValueError:
            rec["node_error"] = (ph.stdout + ph.stderr)[-500:]
        time.sleep(5)
        t1 = time.time()
        code, body = dly.call("POST", f"/v1/sandboxes/{sid}/resume", {})
        rec["resume_post"] = {"code": code, "s": round(time.time() - t1, 2)}
        dly.wait(sid, "running", timeout=600)
        rec["resume_s"] = round(time.time() - t1, 2)
        t2 = time.time(); dly.exe(sid, "true"); rec["first_exec_s"] = round(time.time() - t2, 2)
        rec["resume_to_exec_s"] = round(time.time() - t1, 2)
        after, procs_after = state()
        fds_before, fds_after = set(before.pop("_fds")), set(after.pop("_fds"))
        if fds_before != fds_after:
            # Which process opened or closed what: apps change their files on their own.
            rec["fds_changed"] = {"closed": sorted(fds_before - fds_after)[:20], "opened": sorted(fds_after - fds_before)[:20]}
        rec["state_kept"] = {k: before.get(k) == after.get(k) for k in before}
        rec["procs_kept"] = procs_before == procs_after
        if not all(rec["state_kept"].values()) or not rec["procs_kept"]:
            rec["state_diff"] = {"before": before, "after": after, "procs_before": procs_before, "procs_after": procs_after}
        rec["holder_ok"] = (after.get("holder_mem") or "x y").split()[0] == (after.get("holder_mem") or "x y").split()[1] if after.get("holder_mem") else None
        out.write(json.dumps(rec) + "\n")
        p = (rec.get("node") or {}).get("phases", {})
        print(f"[{tag}] cycle {i}: pause {rec['pause_s']} s (dump {p.get('memory_dump')}, checksum {p.get('checksum_and_manifest')}, upload {p.get('upload')}, commit {p.get('metadata_commit')}), resume {rec['resume_s']} s, exec after {rec['resume_to_exec_s']} s, kept {rec['procs_kept'] and all(rec['state_kept'].values())}", flush=True)
        time.sleep(20)
finally:
    sampler.terminate()
