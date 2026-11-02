#!/usr/bin/env python3
"""Turn the all-zero 64 KiB runs of a file into holes, in place: what Fast
Sandbox's pull would write with the sparse-pull patch. Content is unchanged.
usage (root): punch.py FILE..."""
import ctypes, os, sys
libc = ctypes.CDLL("libc.so.6", use_errno=True)
libc.fallocate.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_long, ctypes.c_long]
PUNCH = 0x02 | 0x01  # FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE
CHUNK, BUF = 64 << 10, 4 << 20
ZERO = bytes(CHUNK)

def punch(fd, off, n):
    if n and libc.fallocate(fd, PUNCH, off, n) != 0:
        raise OSError(ctypes.get_errno(), "fallocate")

for path in sys.argv[1:]:
    st = os.stat(path)
    fd = os.open(path, os.O_RDWR)
    run_start, run_len, off = None, 0, 0
    with os.fdopen(fd, "r+b", buffering=0) as f:
        while True:
            buf = f.read(BUF)
            if not buf:
                break
            for i in range(0, len(buf), CHUNK):
                chunk = buf[i:i + CHUNK]
                if chunk == ZERO[:len(chunk)]:
                    if run_start is None:
                        run_start, run_len = off + i, 0
                    run_len += len(chunk)
                elif run_start is not None:
                    punch(fd, run_start, run_len); run_start = None
            off += len(buf)
        if run_start is not None:
            punch(fd, run_start, run_len)
        os.fsync(fd)
    after = os.stat(path)
    print(f"{path}: {st.st_size/2**30:.2f} GiB, allocated {st.st_blocks*512/2**30:.2f} -> {after.st_blocks*512/2**30:.2f} GiB")
