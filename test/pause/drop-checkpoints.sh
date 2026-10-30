#!/bin/sh
# Remove consumed checkpoint sets from Fast Sandbox's node image cache (what
# its cache GC does): entries whose manifest records a checkpoint owner.
# Templates and golden images stay. Run as root, and only with no sandbox
# alive: a sandbox resumed from a checkpoint needs that checkpoint's
# manifest to take its next one.
cd /var/lib/fast-sandbox/firecracker/images || exit 1
for d in */; do
  d=${d%/}
  if grep -q '"checkpoint"' "$d/manifest.json" 2>/dev/null; then rm -rf "$d" && echo "dropped $d"; fi
done
df -h /var/lib/fast-sandbox | tail -1
