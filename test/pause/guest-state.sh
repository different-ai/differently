# Prints a fingerprint of the guest: long-lived processes (pid, start, name),
# their open files, and checksums of memory and disk contents.
pids=$(for p in /proc/[0-9]*; do n=$(cat $p/comm 2>/dev/null); case "$n" in Xvfb|openbox|soffice.bin|chromium|python3|execd|sandbox-init) echo ${p#/proc/};; esac; done)
for p in $pids; do
  set -- $(sed 's/^.*) //' /proc/$p/stat 2>/dev/null); st=${20}
  [ -n "$st" ] && echo "proc $p $(cat /proc/$p/comm) $st"
done | grep -v " python3 .*" > /tmp/procs.txt
hp=$(cat /root/holder.pid 2>/dev/null)
[ -n "$hp" ] && echo "proc $hp holder $(sed 's/^.*) //' /proc/$hp/stat | awk '{print $20}')" >> /tmp/procs.txt
sort /tmp/procs.txt
echo "procs_sha $(sort /tmp/procs.txt | sha256sum | cut -c1-16)"
# open files of the desktop apps (execd's own exec connections change per call)
fpids=$(for p in $pids $hp; do [ "$(cat /proc/$p/comm 2>/dev/null)" = execd ] || echo $p; done)
for p in $fpids; do ls -l /proc/$p/fd 2>/dev/null | awk -v p=$p 'NR>1{print p, $NF}'; done | sort > /tmp/fds.txt
sed 's/^/fd /' /tmp/fds.txt
echo "fds_count $(wc -l < /tmp/fds.txt)"
echo "fds_sha $(sha256sum < /tmp/fds.txt | cut -c1-16)"
if [ -n "$hp" ]; then rm -f /root/holder.check; kill -USR1 $hp; for i in 1 2 3 4 5 6 7 8 9 10; do [ -s /root/holder.check ] && break; sleep 0.5; done
  echo "holder_mem $(cut -c1-16 /root/holder.sha) $(cut -c1-16 /root/holder.check)"; fi
[ -f /tmp/marker.bin ] && echo "tmpfs_sha $(sha256sum /tmp/marker.bin | cut -c1-16)"
[ -f /workspace/data/rand-0.bin ] && echo "disk_sha $(sha256sum /workspace/data/rand-0.bin | cut -c1-16)"
echo "boot_id $(cat /proc/sys/kernel/random/boot_id)"
