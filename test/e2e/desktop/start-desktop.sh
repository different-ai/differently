#!/bin/sh
# Starts the desktop in the background; with --wait, stays in the foreground.
set -e
export DISPLAY=:1
pgrep -x Xvfb >/dev/null || nohup Xvfb :1 -screen 0 1280x800x24 -nolisten tcp >/tmp/xvfb.log 2>&1 &
sleep 1
pgrep -x openbox >/dev/null || nohup openbox >/tmp/openbox.log 2>&1 &
pgrep -x x11vnc >/dev/null || nohup x11vnc -display :1 -rfbport 5900 -localhost -nopw -forever -shared >/tmp/x11vnc.log 2>&1 &
pgrep -f websockify >/dev/null || nohup websockify 0.0.0.0:6080 localhost:5900 >/tmp/websockify.log 2>&1 &
pgrep -x chromium >/dev/null || nohup chromium --no-sandbox --no-first-run --disable-gpu --user-data-dir=/tmp/chromium \
  "data:text/html,<body style='background:%231e6fd9;color:white;font:48px sans-serif'><h1>Differently microVM desktop</h1>" >/tmp/chromium.log 2>&1 &
[ "$1" = "--wait" ] && exec tail -f /dev/null
exit 0
