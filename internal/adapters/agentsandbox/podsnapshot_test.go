//go:build experimental_adapters

package agentsandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeSnapshots adds GKE Pod Snapshots to fakeKube: a PodSnapshotPolicy, and
// triggers that checkpoint a pod on the next read into a ready PodSnapshot
// holding the pod's marker files, group and pod spec. A pod the controller
// creates is restored as GKE would: from the snapshot its template names, or
// else from the newest one in its group with the same pod spec; only if the
// snapshot is in the pod's group, has the same pod spec, and noRestore is
// unset.
type fakeSnapshots struct {
	*fakeKube
	policy     map[string]any
	snapString bool // status.snapshotCreated as a string rather than {name}
	failNext   bool // the next checkpoint fails
	noRestore  bool // GKE starts restored pods fresh (another machine series, say)
	snaps      map[string]map[string]any
	triggers   map[string]map[string]any
	seq        int // orders snapshots, newest last
}

func validPolicy() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "dly"},
		"spec": map[string]any{
			"selector":              map[string]any{"matchExpressions": []any{map[string]any{"key": snapshotGroup, "operator": "Exists"}}},
			"storageConfigName":     "gcs",
			"triggerConfig":         map[string]any{"type": "manual", "postCheckpoint": "resume"},
			"snapshotGroupingRules": map[string]any{"groupByLabelValue": map[string]any{"labels": []any{snapshotGroup}}},
		},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "StorageConfigValid"}}},
	}
}

func newFakeSnapshots(t *testing.T) *fakeSnapshots {
	f := &fakeSnapshots{fakeKube: newFakeKube(t), policy: validPolicy(), snaps: map[string]map[string]any{}, triggers: map[string]map[string]any{}}
	jsonOut := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func(w http.ResponseWriter, what string) {
		jsonOut(w, 404, map[string]any{"kind": "Status", "status": "Failure", "reason": "NotFound", "message": what + " not found", "code": 404})
	}
	locked := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	const base = "/apis/podsnapshot.gke.io/v1/namespaces/ns1/"
	f.mux.HandleFunc("GET "+base+"podsnapshotpolicies/{name}", locked(func(w http.ResponseWriter, r *http.Request) {
		if f.policy == nil || r.PathValue("name") != "dly" {
			notFound(w, "podsnapshotpolicies "+r.PathValue("name"))
			return
		}
		jsonOut(w, 200, f.policy)
	}))
	f.mux.HandleFunc("POST "+base+"podsnapshotmanualtriggers", locked(func(w http.ResponseWriter, r *http.Request) {
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		f.triggers[obj["metadata"].(map[string]any)["name"].(string)] = obj
		jsonOut(w, 201, obj)
	}))
	f.mux.HandleFunc("GET "+base+"podsnapshotmanualtriggers", locked(func(w http.ResponseWriter, r *http.Request) {
		want := map[string]string{}
		for _, kv := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
			k, v, _ := strings.Cut(kv, "=")
			want[k] = v
		}
		items := []any{}
		for _, t := range f.triggers {
			labels := t["metadata"].(map[string]any)["labels"].(map[string]any)
			ok := true
			for k, v := range want {
				ok = ok && labels[k] == v
			}
			if ok {
				items = append(items, t)
			}
		}
		jsonOut(w, 200, map[string]any{"items": items})
	}))
	f.mux.HandleFunc("GET "+base+"podsnapshotmanualtriggers/{name}", locked(func(w http.ResponseWriter, r *http.Request) {
		t, ok := f.triggers[r.PathValue("name")]
		if !ok {
			notFound(w, "podsnapshotmanualtriggers "+r.PathValue("name"))
			return
		}
		f.checkpoint(r.PathValue("name"), t)
		jsonOut(w, 200, t)
	}))
	for _, plural := range []string{"podsnapshotmanualtriggers", "podsnapshots"} {
		store := map[string]map[string]map[string]any{"podsnapshotmanualtriggers": f.triggers, "podsnapshots": f.snaps}[plural]
		f.mux.HandleFunc("DELETE "+base+plural+"/{name}", locked(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := store[r.PathValue("name")]; !ok {
				notFound(w, plural+" "+r.PathValue("name"))
				return
			}
			delete(store, r.PathValue("name"))
			jsonOut(w, 200, map[string]any{"status": "Success"})
		}))
	}
	f.mux.HandleFunc("GET "+base+"podsnapshots/{name}", locked(func(w http.ResponseWriter, r *http.Request) {
		s, ok := f.snaps[r.PathValue("name")]
		if !ok {
			notFound(w, "podsnapshots "+r.PathValue("name"))
			return
		}
		jsonOut(w, 200, s)
	}))
	f.restore = func(sandbox string, obj map[string]any) map[string]bool {
		tmpl := obj["spec"].(map[string]any)["podTemplate"].(map[string]any)
		md, _ := tmpl["metadata"].(map[string]any)
		ann, _ := md["annotations"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		group := labels[snapshotGroup]
		var snap map[string]any
		if n := str(ann[psName]); n != "" {
			snap = f.snaps[n]
		} else {
			for _, s := range f.snaps {
				if s["group"] == group && sameJSON(s["podSpec"], tmpl["spec"]) && (snap == nil || s["seq"].(int) > snap["seq"].(int)) {
					snap = s
				}
			}
		}
		if snap == nil || f.noRestore || group == nil || snap["group"] != group || !sameJSON(snap["podSpec"], tmpl["spec"]) {
			return map[string]bool{} // a fresh start, as GKE does it: silently
		}
		out := map[string]bool{}
		for k := range snap["markers"].(map[string]bool) {
			out[k] = true
		}
		return out
	}
	return f
}

func str(v any) string { s, _ := v.(string); return s }

func sameJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var x, y any
	_ = json.Unmarshal(ja, &x)
	_ = json.Unmarshal(jb, &y)
	return reflect.DeepEqual(x, y)
}

// checkpoint plays GKE's snapshot controller for one trigger.
func (f *fakeSnapshots) checkpoint(name string, t map[string]any) {
	if t["status"] != nil {
		return
	}
	if f.failNext {
		f.failNext = false
		t["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Triggered", "status": "False", "reason": "CheckpointFailed", "message": "gVisor checkpoint failed"}}}
		return
	}
	pod := t["spec"].(map[string]any)["targetPod"].(string)
	sb := f.objects["sandboxes/"+pod]
	tmpl := sb["spec"].(map[string]any)["podTemplate"].(map[string]any)
	labels := tmpl["metadata"].(map[string]any)["labels"].(map[string]any)
	snap := name + "-ps"
	markers := map[string]bool{}
	for k := range f.markers[pod] {
		markers[k] = true
	}
	// The group is the running pod's label, which the controller copies from
	// the template.
	podLabels := f.pods[pod]["metadata"].(map[string]any)["labels"].(map[string]any)
	if podLabels[snapshotGroup] != labels[snapshotGroup] {
		f.t.Errorf("checkpoint of %s: the pod's group %v differs from its template's %v", pod, podLabels[snapshotGroup], labels[snapshotGroup])
	}
	f.seq++
	f.snaps[snap] = map[string]any{
		"metadata": map[string]any{"name": snap},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "AllSnapshotsAvailable"}}},
		"markers":  markers, "group": podLabels[snapshotGroup], "podSpec": tmpl["spec"], "seq": f.seq,
	}
	var created any = map[string]any{"name": snap}
	if f.snapString {
		created = snap
	}
	t["status"] = map[string]any{"snapshotCreated": created, "conditions": []any{map[string]any{"type": "Triggered", "status": "True", "reason": "Complete"}}}
}

func (f *fakeSnapshots) count() (snaps, triggers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.snaps), len(f.triggers)
}

func snapCfg() map[string]string {
	return map[string]string{"podSnapshotPolicy": "dly", "runtimeClassName": "gvisor", "image": "python:3.13"}
}

func TestFromConfigPodSnapshots(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	for cfg, want := range map[string]string{
		`{"apiServer":"https://k","podSnapshotPolicy":"p"}`:                                                   "need gVisor",
		`{"apiServer":"https://k","podSnapshotPolicy":"p","warmPool":"w"}`:                                    "not offered with config.warmPool",
		`{"apiServer":"https://k","podSnapshotPolicy":"p","runtimeClassName":"gvisor","snapshotTimeout":"x"}`: "snapshotTimeout must be a duration",
	} {
		var m map[string]string
		_ = json.Unmarshal([]byte(cfg), &m)
		if _, err := FromConfig(m, api.TrustPrivate, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", cfg, want, err)
		}
	}
	o, err := FromConfig(map[string]string{"apiServer": "https://k", "podSnapshotPolicy": "p", "runtimeClassName": "gvisor", "snapshotTimeout": "90s"}, api.TrustPrivate, "")
	if err != nil || o.SnapshotTimeout.Seconds() != 90 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, feat := range SnapshotFeatures {
		if !slices.Contains(o.Features, feat) {
			t.Errorf("features %v lack %s", o.Features, feat)
		}
	}
	// Without a policy nothing is advertised, and every call says so.
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	if _, err := a.Snapshot(ctx, "x", "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Errorf("snapshot: %v", err)
	}
	if err := a.Pause(ctx, "x"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Errorf("pause: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_f", Installation: "i", FromSnapshot: "a/b"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Errorf("fork: %v", err)
	}
	got := adapter.EffectiveFeatures(f.adapter(t, snapCfg()), o.Features)
	for _, feat := range SnapshotFeatures {
		if !slices.Contains(got, feat) {
			t.Errorf("effective features %v lack %s: the adapter does not implement it", got, feat)
		}
	}
}

func TestPodSnapshotPolicyIsChecked(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	spec := func(p map[string]any) map[string]any { return p["spec"].(map[string]any) }
	for want, change := range map[string]func(p map[string]any){
		"triggerConfig.type must be manual": func(p map[string]any) {
			spec(p)["triggerConfig"] = map[string]any{"type": "workload"}
		},
		"snapshotScope must be whole-pod": func(p map[string]any) {
			spec(p)["snapshotScope"] = "rootfs-only"
		},
		"postCheckpoint must be resume": func(p map[string]any) {
			spec(p)["triggerConfig"] = map[string]any{"type": "manual", "postCheckpoint": "stop"}
		},
		"selector.matchExpressions must include": func(p map[string]any) {
			spec(p)["selector"] = map[string]any{"matchLabels": map[string]any{"app": "sandbox"}}
		},
		"or a new sandbox could be restored from another sandbox's snapshot": func(p map[string]any) {
			delete(spec(p), "snapshotGroupingRules")
		},
		"maxSnapshotCountPerGroup must be 0": func(p map[string]any) {
			spec(p)["snapshotGroupingRules"] = map[string]any{"groupByLabelValue": map[string]any{"labels": []any{snapshotGroup}, "groupRetentionPolicy": map[string]any{"maxSnapshotCountPerGroup": 3}}}
		},
		"not ready: StorageConfigInvalid": func(p map[string]any) {
			p["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "StorageConfigInvalid"}}}
		},
	} {
		f.mu.Lock()
		f.policy = validPolicy()
		change(f.policy)
		f.mu.Unlock()
		err := a.Health(ctx)
		if !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	f.mu.Lock()
	f.policy = nil
	f.mu.Unlock()
	if err := a.Health(ctx); err == nil || !strings.Contains(err.Error(), "PodSnapshotPolicy dly not found") {
		t.Errorf("missing policy: %v", err)
	}
}

// tmplMeta returns a Sandbox's pod template metadata.
func (f *fakeSnapshots) tmplMeta(ref string) (labels, annotations map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	md := f.objects["sandboxes/"+ref]["spec"].(map[string]any)["podTemplate"].(map[string]any)["metadata"].(map[string]any)
	labels, _ = md["labels"].(map[string]any)
	annotations, _ = md["annotations"].(map[string]any)
	return labels, annotations
}

// token returns the marker token of the checkpoint behind a snapshot ref.
func (f *fakeSnapshots) token(snapRef string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, trig, _ := strings.Cut(snapRef, "/")
	return f.triggers[trig]["metadata"].(map[string]any)["annotations"].(map[string]any)[annoToken].(string)
}

// has reports whether a pod holds the marker of the checkpoint with token.
func (f *fakeSnapshots) has(pod, token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markers[pod][markerPrefix+token]
}

// recreatePod deletes a pod, as a node drain does; the next read of its
// Sandbox makes the controller create it again, and GKE restore it.
func (f *fakeSnapshots) recreatePod(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pods, ref)
	delete(f.markers, ref)
}

var small = api.Resources{CPU: 1, MemoryMiB: 512}

func TestSnapshotAndFork(t *testing.T) {
	ctx := context.Background()
	for _, asString := range []bool{false, true} {
		f := newFakeSnapshots(t)
		f.snapString = asString
		a := f.adapter(t, snapCfg())
		src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_src", Installation: "inst_1", Resources: small})
		if err != nil {
			t.Fatal(err)
		}
		if labels, _ := f.tmplMeta(src.Ref); labels[snapshotGroup] != "dly-sbx-src" {
			t.Fatalf("a new sandbox must have a snapshot group of its own: %v", labels)
		}

		snap, err := a.Snapshot(ctx, src.Ref, "disk")
		if err != nil || snap.Kind != "memory" || !strings.HasPrefix(snap.Ref, "dly-sbx-src-") || !strings.Contains(snap.Ref, "/") {
			t.Fatalf("snapshot %+v %v", snap, err)
		}

		if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_big", Installation: "inst_1", Resources: api.Resources{CPU: 4, MemoryMiB: 512}, FromSnapshot: snap.Ref}); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "ask for cpu 1 and memory 512Mi") {
			t.Fatalf("a fork at another size: %v", err)
		}

		// The fork asks for another image: it gets its source's pod spec,
		// which GKE needs to restore it.
		fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_fork", Installation: "inst_1", Template: "busybox", Resources: small, FromSnapshot: snap.Ref, TTLSeconds: 60})
		if err != nil || fork.State != adapter.StateRunning {
			t.Fatalf("fork %+v %v", fork, err)
		}
		if !f.has(fork.Ref, f.token(snap.Ref)) {
			t.Fatal("the fork was not restored from the snapshot")
		}
		obj := f.object("sandboxes/dly-sbx-fork")
		if obj["metadata"].(map[string]any)["labels"].(map[string]any)[adapter.LabelSandboxID] != "sbx_fork" || obj["spec"].(map[string]any)["shutdownPolicy"] != "Delete" {
			t.Fatalf("fork %v", obj)
		}
		if !sameJSON(obj["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"], f.object("sandboxes/dly-sbx-src")["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"]) {
			t.Fatalf("the fork's pod spec differs from its source's")
		}
		// Once restored, the fork is in a group of its own and names no snapshot.
		if labels, ann := f.tmplMeta(fork.Ref); labels[snapshotGroup] != "dly-sbx-fork" || ann[psName] != nil {
			t.Fatalf("the fork stayed in its source's group or kept naming the snapshot: %v %v", labels, ann)
		}

		// Snapshots outlive their sandbox, and are deleted with their files.
		if err := a.Delete(ctx, src.Ref); err != nil {
			t.Fatal(err)
		}
		if s, tr := f.count(); s != 1 || tr != 1 {
			t.Fatalf("after deleting the source: %d snapshots, %d triggers", s, tr)
		}
		if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
			t.Fatal(err)
		}
		if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
			t.Fatalf("deleting twice: %v", err)
		}
		if s, tr := f.count(); s != 0 || tr != 0 {
			t.Fatalf("after deleting the snapshot: %d snapshots, %d triggers", s, tr)
		}
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_gone", Installation: "inst_1", Resources: small, FromSnapshot: snap.Ref}); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("fork of a deleted snapshot: %v", err)
		}
	}
}

// A sandbox whose pod GKE recreates comes back from its own newest
// checkpoint, never from a fork's, and a new sandbox from nothing.
func TestRecreatedPodsRestoreOnlyTheirOwnCheckpoints(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_a", Installation: "inst_1", Resources: small})
	if err != nil {
		t.Fatal(err)
	}
	snapA, err := a.Snapshot(ctx, src.Ref, "memory")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_b", Installation: "inst_1", Resources: small, FromSnapshot: snapA.Ref})
	if err != nil {
		t.Fatal(err)
	}
	snapB, err := a.Snapshot(ctx, fork.Ref, "memory") // newer than snapA, same pod spec
	if err != nil {
		t.Fatal(err)
	}
	f.recreatePod(src.Ref)
	if _, err := a.Get(ctx, src.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, src.Ref); err != nil { // the pod is back
		t.Fatal(err)
	}
	if f.has(src.Ref, f.token(snapB.Ref)) {
		t.Fatal("the source came back with its fork's memory")
	}
	if !f.has(src.Ref, f.token(snapA.Ref)) {
		t.Fatal("the source did not come back from its own snapshot")
	}
	fresh, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_c", Installation: "inst_1", Resources: small})
	if err != nil {
		t.Fatal(err)
	}
	if f.has(fresh.Ref, f.token(snapA.Ref)) || f.has(fresh.Ref, f.token(snapB.Ref)) {
		t.Fatal("a new sandbox started from another sandbox's snapshot")
	}
}

func TestForkThatGKEDidNotRestoreFails(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_src", Installation: "inst_1", Resources: small})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, src.Ref, "memory")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.noRestore = true
	f.mu.Unlock()
	_, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_fork", Installation: "inst_1", Resources: small, FromSnapshot: snap.Ref})
	if !api.IsCode(err, adapter.CodeSandboxFailed) || !strings.Contains(err.Error(), "started without its snapshot") {
		t.Fatalf("want sandbox_failed, got %v", err)
	}
	if f.object("sandboxes/dly-sbx-fork") != nil {
		t.Fatal("the fresh fork was left behind")
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_x", Installation: "inst_1", FromSnapshot: "no-slash"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("a malformed snapshot ref: %v", err)
	}
}

// Two checkpoints of one sandbox at once each keep their own marker.
func TestConcurrentCheckpoints(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_src", Installation: "inst_1", Resources: small})
	if err != nil {
		t.Fatal(err)
	}
	snaps := make([]adapter.Snapshot, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range snaps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snaps[i], errs[i] = a.Snapshot(ctx, src.Ref, "memory")
		}()
	}
	wg.Wait()
	for i, s := range snaps {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_f" + strconv.Itoa(i), Installation: "inst_1", Resources: small, FromSnapshot: s.Ref}); err != nil {
			t.Fatalf("fork of concurrent snapshot %d: %v", i, err)
		}
	}
}

func TestCheckpointFailure(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_src", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failNext = true
	f.mu.Unlock()
	if _, err := a.Snapshot(ctx, src.Ref, "memory"); !api.IsCode(err, adapter.CodeSandboxFailed) || !strings.Contains(err.Error(), "CheckpointFailed") {
		t.Fatalf("want sandbox_failed, got %v", err)
	}
	if s, tr := f.count(); s != 0 || tr != 0 {
		t.Fatalf("a failed checkpoint left %d snapshots and %d triggers", s, tr)
	}
}

func TestPauseResume(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_p", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 2; round++ {
		if err := a.Pause(ctx, in.Ref); err != nil {
			t.Fatalf("pause %d: %v", round, err)
		}
		if err := a.Pause(ctx, in.Ref); err != nil {
			t.Fatalf("pause %d again: %v", round, err)
		}
		if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StatePaused {
			t.Fatalf("after pause %d: %+v %v", round, got, err)
		}
		if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 || list[0].State != adapter.StatePaused {
			t.Fatalf("list after pause %d: %+v %v", round, list, err)
		}
		f.mu.Lock()
		_, podLeft := f.pods[in.Ref]
		f.mu.Unlock()
		if podLeft {
			t.Fatalf("pause %d kept the pod: a paused sandbox holds no node resources", round)
		}
		if s, tr := f.count(); s != 1 || tr != 1 {
			t.Fatalf("pause %d keeps only the newest checkpoint: %d snapshots, %d triggers", round, s, tr)
		}
		if _, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
			t.Fatalf("exec while paused: %v", err)
		}
		if err := a.Resume(ctx, in.Ref); err != nil {
			t.Fatalf("resume %d: %v", round, err)
		}
		if err := a.Resume(ctx, in.Ref); err != nil {
			t.Fatalf("resume %d again: %v", round, err)
		}
		if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StateRunning {
			t.Fatalf("after resume %d: %+v %v", round, got, err)
		}
		if _, ann := f.tmplMeta(in.Ref); ann[psName] != nil {
			t.Fatalf("after resume %d the template still names the pause checkpoint: a recreated pod would roll back to it", round)
		}
	}
	if err := a.Delete(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if s, tr := f.count(); s != 0 || tr != 0 {
		t.Fatalf("delete left %d pause snapshots and %d triggers", s, tr)
	}
}

// A pause whose suspend does not finish leaves the sandbox running, as the
// scheduler records it.
func TestPauseThatDoesNotSuspendIsUndone(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	a.opts.ReadyTimeout = time.Second
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_p", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.stuckSuspend = true
	f.mu.Unlock()
	if err := a.Pause(ctx, in.Ref); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("want sandbox_failed, got %v", err)
	}
	f.mu.Lock()
	f.stuckSuspend = false
	f.mu.Unlock()
	if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StateRunning {
		t.Fatalf("after a failed pause: %+v %v", got, err)
	}
	if res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("exec after a failed pause: %+v %v", res, err)
	}
}

func TestResumeThatGKEDidNotRestore(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_p", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Pause(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.noRestore = true
	f.mu.Unlock()
	if err := a.Resume(ctx, in.Ref); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("want sandbox_failed, got %v", err)
	}
	if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StatePaused {
		t.Fatalf("a resume without the checkpoint must leave it paused: %+v %v", got, err)
	}
	// Once GKE can restore it (a node of the right series, say), it resumes.
	f.mu.Lock()
	f.noRestore = false
	f.mu.Unlock()
	if err := a.Resume(ctx, in.Ref); err != nil {
		t.Fatalf("second resume: %v", err)
	}

	// A sandbox GKE never restores fails after maxRestoreFailures resumes,
	// rather than being retried for ever.
	if err := a.Pause(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.noRestore = true
	f.mu.Unlock()
	for range maxRestoreFailures {
		if err := a.Resume(ctx, in.Ref); !api.IsCode(err, adapter.CodeSandboxFailed) {
			t.Fatalf("want sandbox_failed, got %v", err)
		}
	}
	got, err := a.Get(ctx, in.Ref)
	if err != nil || got.State != adapter.StateError || !strings.Contains(got.Message, "could not be restored") {
		t.Fatalf("after %d failed resumes: %+v %v", maxRestoreFailures, got, err)
	}
	gen := f.object("sandboxes/" + in.Ref)["metadata"].(map[string]any)["generation"]
	if err := a.Resume(ctx, in.Ref); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("one more resume: %v", err)
	}
	if f.object("sandboxes/" + in.Ref)["metadata"].(map[string]any)["generation"] != gen {
		t.Fatal("a resume past the limit started the pod again")
	}
}

func TestStopStartIsNotOfferedWithSnapshots(t *testing.T) {
	f := newFakeSnapshots(t)
	cfg := snapCfg()
	cfg["volumeSize"] = "1Gi"
	a := f.adapter(t, cfg)
	caps, _ := a.Capabilities(context.Background())
	if slices.Contains(caps.Features, adapter.FeatureStopStart) {
		t.Fatalf("features %v offer stop/start, which would restore the newest checkpoint over a newer volume", caps.Features)
	}
	if err := a.Stop(context.Background(), "x"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stop: %v", err)
	}
}

// Pause checkpoints of a Sandbox deleted behind Differently's back are
// collected by the next List.
func TestListCollectsOrphanedPauseCheckpoints(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	keep, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_keep", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_gone", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{keep.Ref, gone.Ref} {
		if err := a.Pause(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	delete(f.objects, "sandboxes/"+gone.Ref) // the TTL backstop
	f.mu.Unlock()
	if _, err := a.List(ctx, "inst_1"); err != nil {
		t.Fatal(err)
	}
	if s, tr := f.count(); s != 1 || tr != 1 {
		t.Fatalf("after list: %d snapshots, %d triggers; want the kept sandbox's only", s, tr)
	}
	if err := a.Resume(ctx, keep.Ref); err != nil {
		t.Fatalf("the kept sandbox's checkpoint was collected: %v", err)
	}
}
