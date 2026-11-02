# Runs inside the desktop guest. "setup" starts a working desktop session;
# "step N" adds a little more use before each pause.
export DISPLAY=:99 HOME=/root
case "$1" in
setup)
  chmod 0755 /run 2>/dev/null
  mkdir -p /workspace/data /root/chr
  setsid Xvfb :99 -screen 0 1280x800x24 -nolisten tcp >/dev/null 2>&1 < /dev/null &
  sleep 1
  command -v openbox >/dev/null && setsid openbox >/dev/null 2>&1 < /dev/null &
  setsid soffice --writer --norestore --nologo >/dev/null 2>&1 < /dev/null &
  setsid chromium --no-sandbox --no-first-run --disable-gpu --user-data-dir=/root/chr \
    file:///usr/share/doc/ file:///usr/share/common-licenses/ >/dev/null 2>&1 < /dev/null &
  # an app holding state in memory: 384 MiB of random bytes with its checksum
  cat > /root/holder.py <<'PY'
import hashlib, os, signal, time
buf = bytearray(os.urandom(384 << 20))
open("/root/holder.sha", "w").write(hashlib.sha256(buf).hexdigest())
def check(*_):
    open("/root/holder.check", "w").write(hashlib.sha256(buf).hexdigest())
signal.signal(signal.SIGUSR1, check)
while True:
    time.sleep(3600)
PY
  setsid python3 /root/holder.py >/dev/null 2>&1 < /dev/null &
  echo $! > /root/holder.pid
  head -c 256M /dev/urandom > /tmp/marker.bin            # tmpfs: guest RAM
  head -c 300M /dev/urandom > /workspace/data/rand-0.bin  # user files on disk
  cp -r /usr/share/doc /workspace/data/doc-0 2>/dev/null
  find /usr -xdev -type f -size -4M -print0 2>/dev/null | xargs -0 cat > /dev/null 2>&1  # page cache
  sleep 15
  ;;
step)
  head -c 64M /dev/urandom > /workspace/data/rand-$2.bin
  cp -r /usr/share/doc/python3* /workspace/data/ 2>/dev/null
  sync
  ;;
esac
free -m | awk '/Mem/{print "guest mem used MiB", $3, "cache MiB", $6}'
df -m / | awk 'NR==2{print "guest disk used MiB", $3}'
