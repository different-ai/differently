#!/usr/bin/env python3
"""Sample the state-root disk and the pause path's processes once a second.
Writes JSON lines {t, rd_mb, wr_mb, busy_ms, cpu: {name: seconds}}, each a
delta over the last second. Run as root: sampler.py OUT [DEVICE]"""
import json, os, sys, time

out = open(sys.argv[1], "a", buffering=1)
dev = sys.argv[2] if len(sys.argv) > 2 else "nvme0n1"
HZ = os.sysconf("SC_CLK_TCK")
# Process name prefixes (comm is cut at 15 bytes), most specific first.
NAMES = [("firecracker-run", "agent"), ("firecracker", "firecracker"), ("garage", "garage"), ("fastlet", "fastlet")]


def disk():
    for line in open("/proc/diskstats"):
        f = line.split()
        if f[2] == dev:
            return int(f[5]) * 512, int(f[9]) * 512, int(f[12])
    return 0, 0, 0


def procs():
    ticks = {}
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            comm = open(f"/proc/{pid}/comm").read().strip()
            label = next((v for k, v in NAMES if comm.startswith(k)), None)
            if label is None:
                continue
            st = open(f"/proc/{pid}/stat").read().rsplit(")", 1)[1].split()
            ticks[pid] = (label, int(st[11]) + int(st[12]))
        except (FileNotFoundError, ProcessLookupError, PermissionError, IndexError):
            pass
    return ticks


pr, pw, pb = disk()
pc = procs()
while True:
    time.sleep(1)
    r, w, b = disk()
    c = procs()
    cpu = {}
    for pid, (label, t) in c.items():
        cpu[label] = cpu.get(label, 0) + (t - pc.get(pid, (label, 0))[1]) / HZ  # a new process counts from 0
    out.write(json.dumps({"t": round(time.time(), 3), "rd_mb": round((r - pr) / 1e6, 2), "wr_mb": round((w - pw) / 1e6, 2),
                          "busy_ms": b - pb, "cpu": {k: round(v, 2) for k, v in cpu.items()}}) + "\n")
    pr, pw, pb, pc = r, w, b, c
