# A small desktop for the e2e desktop test: the same stack as OpenWork's
# headless computer (Xvfb, openbox, x11vnc, chromium) with websockify serving
# VNC over a WebSocket on port 6080, at any path (such as /vnc/<token>).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    xvfb openbox x11vnc chromium websockify procps ca-certificates fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*
COPY start-desktop.sh /usr/local/bin/start-desktop
RUN chmod 0755 /usr/local/bin/start-desktop
CMD ["/usr/local/bin/start-desktop", "--wait"]
