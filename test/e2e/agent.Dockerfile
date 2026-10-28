# The released agent image plus a guest kernel and rootfs, for the e2e stack.
ARG AGENT_IMAGE=differently-agent:e2e
FROM ${AGENT_IMAGE}
COPY .artifacts/images/vmlinux .artifacts/images/default.ext4 /var/lib/differently/images/
