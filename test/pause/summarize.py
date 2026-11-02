#!/usr/bin/env python3
"""Summarize pause/resume runs (cycle.py output) as Markdown tables.
usage: summarize.py RUN.jsonl..."""
import json, math, statistics, sys

def pct(xs, q):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    return xs[max(0, min(len(xs) - 1, math.ceil(q * len(xs)) - 1))]

def fmt(x, unit=" s", nd=1):
    return "–" if x is None else f"{x:.{nd}f}{unit}"

def at_floor(r):
    """A burstable disk out of credits: a phase kept the disk busy but moved
    far less than its burst rate (about 175 MB/s on a P6 or P15)."""
    node = r.get("node") or {}
    for ph in ("memory_dump", "checksum_and_manifest", "upload"):
        d, secs = (node.get("disk") or {}).get(ph) or {}, (node.get("phases") or {}).get(ph)
        if not secs or secs < 10:
            continue
        busy, rate = d.get("disk_busy_pct", 0), (d.get("read_mb", 0) + d.get("write_mb", 0)) / secs
        if (busy >= 95 and rate < 150) or (busy >= 85 and rate < 80):
            return True
    return False


runs = {}
for path in sys.argv[1:]:
    for line in open(path):
        r = json.loads(line)
        tag = r["tag"] + (" (disk at its floor)" if at_floor(r) else "")
        runs.setdefault(tag, []).append(r)

print("| Run | Cycles | Pause median | Pause p95 | Resume median | Resume p95 | Resume to first exec, median | State kept |")
print("|---|---:|---:|---:|---:|---:|---:|---|")
for tag, rs in runs.items():
    kept = sum(1 for r in rs if r.get("procs_kept") and all(r.get("state_kept", {}).values()))
    p = [r["pause_s"] for r in rs]; q = [r["resume_s"] for r in rs]; e = [r["resume_to_exec_s"] for r in rs]
    print(f"| {tag} | {len(rs)} | {fmt(statistics.median(p))} | {fmt(pct(p, .95))} | {fmt(statistics.median(q), nd=2)} | {fmt(pct(q, .95), nd=2)} | {fmt(statistics.median(e), nd=2)} | {kept}/{len(rs)} |")

phases = ["quiesce_and_disk_clone", "memory_dump", "checksum_and_manifest", "upload", "metadata_commit", "node_total"]
print()
print("| Run | " + " | ".join(phases) + " |")
print("|---|" + "---:|" * len(phases))
for tag, rs in runs.items():
    cells = []
    for ph in phases:
        xs = [(r.get("node") or {}).get("phases", {}).get(ph) for r in rs]
        cells.append(f"{fmt(statistics.median([x for x in xs if x is not None]) if any(x is not None for x in xs) else None, nd=2)} / {fmt(pct(xs, .95), '', 1)}")
    print(f"| {tag} | " + " | ".join(cells) + " |")

print()
print("| Run | Phase | Disk read MB | Disk write MB | MB/s through disk | Busy | Garage CPU s |")
print("|---|---|---:|---:|---:|---:|---:|")
for tag, rs in runs.items():
    for ph in ["memory_dump", "checksum_and_manifest", "upload", "metadata_commit"]:
        rd = [((r.get("node") or {}).get("disk", {}).get(ph) or {}).get("read_mb") for r in rs]
        wr = [((r.get("node") or {}).get("disk", {}).get(ph) or {}).get("write_mb") for r in rs]
        bz = [((r.get("node") or {}).get("disk", {}).get(ph) or {}).get("disk_busy_pct") for r in rs]
        gc = [(((r.get("node") or {}).get("disk", {}).get(ph) or {}).get("cpu_s") or {}).get("garage") for r in rs]
        du = [(r.get("node") or {}).get("phases", {}).get(ph) for r in rs]
        if not any(x is not None for x in rd):
            continue
        rate = [((a or 0) + (b or 0)) / d for a, b, d in zip(rd, wr, du) if d]
        print(f"| {tag} | {ph} | {statistics.median([x for x in rd if x is not None]):.0f} | {statistics.median([x for x in wr if x is not None]):.0f} | {statistics.median(rate) if rate else 0:.0f} | {statistics.median([x for x in bz if x is not None]):.0f}% | {statistics.median([x for x in gc if x is not None]):.0f} |")

print()
print("| Run | Logical bytes per pause | Garage stored per pause (median) | Upload, rootfs.ext4 / memory.snap (median) |")
print("|---|---:|---:|---:|")
for tag, rs in runs.items():
    lb = [(r.get("node") or {}).get("bytes_logical") for r in rs]
    gs = [r.get("garage_stored_mb") for r in rs]
    ur = [((r.get("node") or {}).get("upload_files") or {}).get("rootfs.ext4") for r in rs]
    um = [((r.get("node") or {}).get("upload_files") or {}).get("memory.snap") for r in rs]
    def med(xs):
        xs = [x for x in xs if x is not None]
        return statistics.median(xs) if xs else None
    print(f"| {tag} | {fmt(med(lb) / 1e9 if med(lb) else None, ' GB', 2)} | {fmt(med(gs), ' MB', 0)} | {fmt(med(ur))} / {fmt(med(um))} |")
