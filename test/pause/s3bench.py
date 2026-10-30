#!/usr/bin/env python3
"""Upload one file to an S3 store the way Fast Sandbox's agent does (one PUT,
UNSIGNED-PAYLOAD) or as a parallel multipart upload. stdlib only.
env: S3_ENDPOINT (http://host:port), S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
usage: s3bench.py FILE KEY single | multipart PART_MiB CONCURRENCY | delete"""
import concurrent.futures as cf, datetime, hashlib, hmac, http.client, os, re, sys, time, urllib.parse

EP = urllib.parse.urlparse(os.environ["S3_ENDPOINT"]); BUCKET = os.environ["S3_BUCKET"]
AK, SK, REGION = os.environ["AWS_ACCESS_KEY_ID"], os.environ["AWS_SECRET_ACCESS_KEY"], os.environ.get("S3_REGION", "us-east-1")

def sign(method, path, query="", payload="UNSIGNED-PAYLOAD", extra=None):
    now = datetime.datetime.now(datetime.timezone.utc); amz = now.strftime("%Y%m%dT%H%M%SZ"); day = amz[:8]
    headers = {"host": EP.netloc, "x-amz-date": amz, "x-amz-content-sha256": payload, **(extra or {})}
    names = sorted(headers)
    canon = "\n".join([method, urllib.parse.quote(path), query, "".join(f"{k}:{headers[k]}\n" for k in names), ";".join(names), payload])
    scope = f"{day}/{REGION}/s3/aws4_request"
    sts = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canon.encode()).hexdigest()])
    k = ("AWS4" + SK).encode()
    for part in (day, REGION, "s3", "aws4_request"):
        k = hmac.new(k, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = f"AWS4-HMAC-SHA256 Credential={AK}/{scope}, SignedHeaders={';'.join(names)}, Signature={sig}"
    return headers

def request(method, path, query="", body=None, length=None):
    h = sign(method, path, query)
    if length is not None:
        h["Content-Length"] = str(length)
    c = http.client.HTTPConnection(EP.hostname, EP.port, timeout=3600, blocksize=1 << 20)
    c.request(method, path + ("?" + query if query else ""), body=body, headers=h)
    r = c.getresponse(); data = r.read(); c.close()
    if r.status >= 300:
        raise SystemExit(f"{method} {path}?{query} -> {r.status} {data[:300]}")
    return r, data

class Section:
    def __init__(self, f, off, n): self.f, self.off, self.left = f, off, n; f.seek(off)
    def read(self, size=-1):
        if self.left <= 0: return b""
        size = self.left if size < 0 else min(size, self.left)
        b = self.f.read(size); self.left -= len(b); return b

def main():
    path, key, mode = sys.argv[1], sys.argv[2], sys.argv[3]
    obj = f"/{BUCKET}/{key}"
    if mode == "delete":
        request("DELETE", obj); return
    size = os.path.getsize(path); t0 = time.time()
    if mode == "single":
        with open(path, "rb") as f:
            request("PUT", obj, body=f, length=size)
    else:
        part = int(sys.argv[4]) << 20; conc = int(sys.argv[5])
        _, data = request("POST", obj, "uploads=")
        upload = re.search(rb"<UploadId>(.*?)</UploadId>", data)[1].decode()
        def put(i):
            off = i * part; n = min(part, size - off)
            with open(path, "rb") as f:
                r, _ = request("PUT", obj, f"partNumber={i+1}&uploadId={urllib.parse.quote(upload)}", body=Section(f, off, n), length=n)
            return i + 1, r.getheader("ETag")
        n = (size + part - 1) // part
        with cf.ThreadPoolExecutor(conc) as ex:
            etags = sorted(ex.map(put, range(n)))
        body = ("<CompleteMultipartUpload>" + "".join(f"<Part><PartNumber>{i}</PartNumber><ETag>{e}</ETag></Part>" for i, e in etags) + "</CompleteMultipartUpload>").encode()
        request("POST", obj, f"uploadId={urllib.parse.quote(upload)}", body=body, length=len(body))
    dt = time.time() - t0
    print(f"{mode} {size/1e6:.0f} MB in {dt:.1f} s = {size/1e6/dt:.0f} MB/s")

main()
