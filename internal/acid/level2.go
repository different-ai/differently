package acid

import (
	"bytes"
	"crypto/sha256"
	"path"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Level 2: files in and out, byte for byte.
func level2() []Test {
	return []Test{
		{ID: "L2.01", Level: 2, Title: "Round trip", Feature: adapter.FeatureFiles, Run: fileRoundTrip},
		{ID: "L2.02", Level: 2, Title: "Seen from inside", Feature: adapter.FeatureFiles, Run: seenFromInside},
		{ID: "L2.03", Level: 2, Title: "Awkward paths", Feature: adapter.FeatureFiles, Run: awkwardPaths},
		{ID: "L2.04", Level: 2, Title: "Listing", Feature: adapter.FeatureFiles, Run: listing},
		{ID: "L2.05", Level: 2, Title: "Large file", Feature: adapter.FeatureFiles, Run: largeFile},
		{ID: "L2.06", Level: 2, Title: "Missing file", Feature: adapter.FeatureFiles, Run: missingFile},
	}
}

func files(t *T) adapter.FileSystem {
	fs, ok := t.Adapter().(adapter.FileSystem)
	if !ok {
		t.Unsupported("no file operations")
	}
	return fs
}

// dir is a fresh directory for one test.
func dir(t *T) string { return "/tmp/" + t.Name("acid-files") }

func write(t *T, ref, p string, data []byte) {
	if err := files(t).WriteFile(t.Ctx(), ref, p, data); err != nil {
		t.Fail("write %s: %s", p, describe(err))
	}
}

func read(t *T, ref, p string) []byte {
	data, err := files(t).ReadFile(t.Ctx(), ref, p)
	if err != nil {
		t.Fail("read %s: %s", p, describe(err))
	}
	return data
}

// payload is n seeded bytes, every byte value present.
func payload(t *T, n int) []byte {
	b := make([]byte, n)
	t.r.mu.Lock()
	for i := range b {
		b[i] = byte(t.r.rng.Uint32())
	}
	t.r.mu.Unlock()
	for i := 0; i < 256 && i < n; i++ {
		b[i] = byte(i)
	}
	return b
}

func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func fileRoundTrip(t *T) {
	ref := t.Shared()
	p := dir(t) + "/roundtrip.bin"
	want := payload(t, 256<<10)
	write(t, ref, p, want)
	got := read(t, ref, p)
	if !bytes.Equal(got, want) {
		t.Fail("read back %d bytes for %d written, first difference at byte %d", len(got), len(want), firstDifference(got, want))
	}
	t.Note("256 KiB of binary (every byte value) reads back identical, into a directory that did not exist")
}

func seenFromInside(t *T) {
	ref := t.Shared()
	d := dir(t)
	content := t.Name("acid-content") + "\n"
	write(t, ref, d+"/from-api.txt", []byte(content))
	if res := t.Sh(ref, "cat "+d+"/from-api.txt"); res.Stdout != content {
		t.Fail("a file written through the API reads as %s from inside, want %s", clip(res.Stdout), clip(content))
	}
	t.Sh(ref, "printf inside > "+d+"/from-exec.txt")
	if got := read(t, ref, d+"/from-exec.txt"); string(got) != "inside" {
		t.Fail("a file written by a command reads as %s through the API", clip(string(got)))
	}
	t.Note("the API and commands see the same filesystem")
}

func awkwardPaths(t *T) {
	ref := t.Shared()
	d := dir(t) + "/dir with space/ünïcødé/deep"
	p := d + "/a b ✓ 'q'.txt"
	write(t, ref, p, []byte("awkward"))
	if got := read(t, ref, p); string(got) != "awkward" {
		t.Fail("%s reads back as %s", clip(p), clip(string(got)))
	}
	list, err := files(t).ListFiles(t.Ctx(), ref, d)
	if err != nil {
		t.Fail("list %q: %s", d, describe(err))
	}
	for _, f := range list {
		if baseName(f) == "a b ✓ 'q'.txt" {
			t.Note("spaces, quotes, UTF-8 and nested new directories work")
			return
		}
	}
	t.Fail("listing %q does not show the file (got %d entries)", d, len(list))
}

func baseName(f api.FileInfo) string {
	if f.Name != "" && !strings.Contains(f.Name, "/") {
		return f.Name
	}
	return path.Base(f.Path)
}

func listing(t *T) {
	ref := t.Shared()
	d := dir(t)
	write(t, ref, d+"/a.txt", []byte("abc"))
	write(t, ref, d+"/b.bin", payload(t, 1000))
	write(t, ref, d+"/sub/c.txt", []byte("c"))
	list, err := files(t).ListFiles(t.Ctx(), ref, d)
	if err != nil {
		t.Fail("list: %s", describe(err))
	}
	seen := map[string]api.FileInfo{}
	for _, f := range list {
		seen[baseName(f)] = f
	}
	if f, ok := seen["a.txt"]; !ok || f.Size != 3 || f.IsDir {
		t.Fail("a.txt listed as %+v, want a 3-byte file", f)
	}
	if f, ok := seen["b.bin"]; !ok || f.Size != 1000 || f.IsDir {
		t.Fail("b.bin listed as %+v, want a 1000-byte file", f)
	}
	if f, ok := seen["sub"]; !ok || !f.IsDir {
		t.Fail("sub listed as %+v, want a directory", f)
	}
	if _, ok := seen["c.txt"]; ok {
		t.Note("the listing is recursive")
	}
	t.Note("names, sizes and directories are reported")
}

func largeFile(t *T) {
	ref := t.Shared()
	n := t.Options().LargeFileBytes
	p := dir(t) + "/large.bin"
	want := payload(t, n)
	start := time.Now()
	write(t, ref, p, want)
	up := time.Since(start).Seconds()
	start = time.Now()
	got := read(t, ref, p)
	down := time.Since(start).Seconds()
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fail("read back %d bytes for %d written, first difference at byte %d", len(got), n, firstDifference(got, want))
	}
	mib := float64(n) / (1 << 20)
	t.Measure("uploadMiBps", mib/up)
	t.Measure("downloadMiBps", mib/down)
	t.Note("%.0f MiB round-trips: up %.1f MiB/s, down %.1f MiB/s", mib, mib/up, mib/down)
	_, _ = t.Adapter().Exec(t.Ctx(), ref, api.ExecRequest{Command: "rm -f " + p, TimeoutSeconds: 60})
}

func missingFile(t *T) {
	p := dir(t) + "/does-not-exist"
	data, err := files(t).ReadFile(t.Ctx(), t.Shared(), p)
	switch {
	case api.IsCode(err, api.CodeNotFound):
		t.Note("a missing file answers not_found")
	case err == nil:
		t.Dishonest("a missing file reads as %d bytes of content instead of not_found", len(data))
	default:
		t.Fail("a missing file answers %s, want not_found", describe(err))
	}
}
