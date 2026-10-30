#!/usr/bin/env python3
"""Split one Fast Sandbox pause into phases from its components' logs
(controller, fastlet, runtime agent, Garage) and the sampler's disk trace.
usage: phases.py SINCE_ISO FAST_SANDBOX_NAME [SAMPLES.jsonl]  (root, on the node)
Prints one JSON object: timestamps, phase durations (s), bytes, disk traffic."""
import datetime as dt, json, os, re, subprocess, sys

since, name = sys.argv[1], sys.argv[2]
samples = sys.argv[3] if len(sys.argv) > 3 else None
K = ["k3s", "kubectl"]
SYSTEM = os.environ.get("FSB_SYSTEM_NAMESPACE", "opensandbox-system")
DATAPLANE = os.environ.get("FSB_DATAPLANE_NAMESPACE", "opensandbox-dataplane")
YEAR = dt.datetime.now(dt.timezone.utc).year  # klog lines carry no year

def logs(ns, target, container=None):
    cmd = K + ["-n", ns, "logs", target, "--since-time", since, "--timestamps=false"]
    if container:
        cmd += ["-c", container]
    return subprocess.run(cmd, capture_output=True, text=True).stdout.splitlines()

def pod(ns, prefix):
    out = subprocess.run(K + ["-n", ns, "get", "pods", "-o", "name"], capture_output=True, text=True).stdout.split()
    return next(p for p in out if p.split("/", 1)[1].startswith(prefix))

KLOG = re.compile(r"^[IWE](\d\d)(\d\d) (\d\d:\d\d:\d\d\.\d+)")
def kt(line):
    m = KLOG.match(line)
    if not m:
        return None
    return dt.datetime.strptime(f"{YEAR}-{m[1]}-{m[2]} {m[3][:15]}", "%Y-%m-%d %H:%M:%S.%f").replace(tzinfo=dt.timezone.utc).timestamp()

def kv(line, key):
    m = re.search(key + r'="?([^" ]+)"?', line)
    return m[1] if m else None

def dur(s):
    if s is None:
        return None
    total, units = 0.0, {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "µs": 1e-6, "us": 1e-6, "ns": 1e-9}
    for num, unit in re.findall(r"([\d.]+)(h|ms|µs|us|ns|m|s)", s):
        total += float(num) * units[unit]
    return total

ctl = logs(SYSTEM, "deploy/fast-sandbox-controller")
fast = logs(DATAPLANE, pod(DATAPLANE, "fsb-pool-fastlet"), "fastlet")
agent = logs(SYSTEM, pod(SYSTEM, "firecracker-runtime"), "firecracker-agent")
garage = logs(SYSTEM, "deploy/fsb-garage")

r = {"sandbox": name}
uid = None
for l in ctl:
    if f"Sandbox=\"{DATAPLANE}/{name}\"" in l or f"sandbox=\"{name}\"" in l:
        uid = uid or kv(l, "sandbox_uid")
        if "pause checkpoint persisted" in l:
            r["t_persisted"] = kt(l)
        elif "sandbox paused; runtime released" in l:
            r["t_released"] = kt(l)
req = [kt(l) for l in ctl if "fastpath sandbox pause requested" in l]
r["sandbox_uid"] = uid
snap = None
for l in fast:
    if uid and f'sandboxID="{uid}"' in l:
        if "dump starting" in l:
            r["t_dump_start"], snap = kt(l), kv(l, "snapshotId")
        elif "paused for dump" in l:
            r["t_vm_paused"] = kt(l)
        elif "resumed after dump" in l:
            r["t_dump_done"] = kt(l)
            for k in ("pauseAPI", "dumpAPI", "rootfsClone", "rootfsCopy", "resumeAPI"):
                r[k] = dur(kv(l, k))
        elif "manifest assembled" in l:
            r["t_manifest"], r["bytes_logical"] = kt(l), int(kv(l, "sizeBytes"))
        elif "snapshot published" in l:
            r["t_published"], r["publish"] = kt(l), dur(kv(l, "publish"))
if "t_dump_start" in r:
    before = [t for t in req if t <= r["t_dump_start"] + 0.5]
    r["t_requested"] = before[-1] if before else None
for l in agent:
    if snap and snap in l and "publish started" in l:
        r["t_upload_start"], digest = kt(l), kv(l, "digest")
        r["namespace"] = digest[:16]
    elif r.get("namespace") and r["namespace"] in l and "publish completed" in l:
        r["t_upload_done"] = kt(l)
GT = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d+)Z")
files = []
if r.get("namespace"):
    for l in garage:
        clean = re.sub(r"\x1b\[[0-9;]*m", "", l)
        if f"PUT /sandbox-images/publish/{r['namespace']}/" in clean:
            m = GT.search(clean)
            t = dt.datetime.strptime(m[1][:26], "%Y-%m-%dT%H:%M:%S.%f").replace(tzinfo=dt.timezone.utc).timestamp()
            files.append((t, clean.rsplit("/", 1)[1].strip()))
files.sort()
ends = [t for t, _ in files[1:]] + [r.get("t_upload_done")]
r["upload_files"] = {f: round(e - t, 2) for (t, f), e in zip(files, ends) if e}

def d(a, b):
    return round(r[b] - r[a], 2) if r.get(a) and r.get(b) else None
r["phases"] = {
    "request_to_node": d("t_requested", "t_dump_start"),
    "quiesce_and_disk_clone": d("t_dump_start", "t_vm_paused"),
    "memory_dump": round(r["dumpAPI"], 2) if r.get("dumpAPI") else None,
    "checksum_and_manifest": d("t_dump_done", "t_manifest"),
    "upload": d("t_upload_start", "t_upload_done"),
    "upload_handoff": round((r.get("t_upload_start") or 0) - (r.get("t_manifest") or 0), 2) if r.get("t_upload_start") else None,
    "metadata_commit": d("t_published", "t_released"),
    "node_total": d("t_dump_start", "t_published"),
}
if samples and r.get("t_dump_start"):
    rows = [json.loads(x) for x in open(samples)]
    marks = [("memory_dump", "t_vm_paused", "t_dump_done"), ("checksum_and_manifest", "t_dump_done", "t_manifest"),
             ("upload", "t_upload_start", "t_upload_done"), ("metadata_commit", "t_published", "t_released")]
    traffic = {}
    for label, a, b in marks:
        if r.get(a) and r.get(b):
            sel = [x for x in rows if r[a] < x["t"] <= r[b] + 1]
            cpu = {}
            for x in sel:
                for k, v in x["cpu"].items():
                    cpu[k] = round(cpu.get(k, 0) + v, 1)
            traffic[label] = {"read_mb": round(sum(x["rd_mb"] for x in sel)), "write_mb": round(sum(x["wr_mb"] for x in sel)),
                              "disk_busy_pct": round(100 * sum(x["busy_ms"] for x in sel) / max(1, 1000 * len(sel))), "cpu_s": cpu}
    r["disk"] = traffic
print(json.dumps(r))
