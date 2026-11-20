//go:build experimental_adapters

package agentsandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeSnapshots adds GKE Pod Snapshots to fakeKube: a PodSnapshotPolicy, and
// triggers that checkpoint a pod on the next read into a ready PodSnapshot
// holding the pod's marker, group and pod spec. A pod the controller creates
// is restored from the snapshot its template names when GKE would: same
// group, same pod spec, ready, and noRestore unset.
type fakeSnapshots struct {
	*fakeKube
	policy     map[string]any
	snapString bool // status.snapshotCreated as a string rather than {name}
	failNext   bool // the next checkpoint fails
	noRestore  bool // GKE starts restored pods fresh (another machine series, say)
	snaps      map[string]map[string]any
	triggers   map[string]map[string]any
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
	f.restore = func(sandbox string, obj map[string]any) string {
		tmpl := obj["spec"].(map[string]any)["podTemplate"].(map[string]any)
		md, _ := tmpl["metadata"].(map[string]any)
		ann, _ := md["annotations"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		snap, ok := f.snaps[str(ann[psName])]
		if !ok || f.noRestore || snap["group"] != labels[snapshotGroup] || !sameJSON(snap["podSpec"], tmpl["spec"]) {
			return "" // a fresh start, as GKE does it: silently
		}
		return snap["marker"].(string)
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
	f.snaps[snap] = map[string]any{
		"metadata": map[string]any{"name": snap},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "AllSnapshotsAvailable"}}},
		"marker":   f.markers[pod], "group": labels[snapshotGroup], "podSpec": tmpl["spec"],
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

func TestSnapshotAndFork(t *testing.T) {
	ctx := context.Background()
	for _, asString := range []bool{false, true} {
		f := newFakeSnapshots(t)
		f.snapString = asString
		a := f.adapter(t, snapCfg())
		src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_src", Installation: "inst_1", Resources: api.Resources{CPU: 1, MemoryMiB: 512}})
		if err != nil {
			t.Fatal(err)
		}
		labels := f.object("sandboxes/dly-sbx-src")["spec"].(map[string]any)["podTemplate"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
		if labels[snapshotGroup] != "dly-sbx-src" {
			t.Fatalf("a new sandbox must have a snapshot group of its own: %v", labels)
		}

		snap, err := a.Snapshot(ctx, src.Ref, "disk")
		if err != nil || snap.Kind != "memory" || !strings.HasPrefix(snap.Ref, "dly-sbx-src-") || !strings.Contains(snap.Ref, "/") {
			t.Fatalf("snapshot %+v %v", snap, err)
		}

		// The fork asks for another image and size: it gets its source's pod
		// spec, which GKE needs to restore it, and its source's group.
		fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_fork", Installation: "inst_1", Template: "busybox", Resources: api.Resources{CPU: 4}, FromSnapshot: snap.Ref, TTLSeconds: 60})
		if err != nil || fork.State != adapter.StateRunning {
			t.Fatalf("fork %+v %v", fork, err)
		}
		obj := f.object("sandboxes/dly-sbx-fork")
		tmpl := obj["spec"].(map[string]any)["podTemplate"].(map[string]any)
		md := tmpl["metadata"].(map[string]any)
		snapName, _, _ := strings.Cut(snap.Ref, "/")
		if md["annotations"].(map[string]any)[psName] != snapName || md["labels"].(map[string]any)[snapshotGroup] != "dly-sbx-src" ||
			md["labels"].(map[string]any)[adapter.LabelSandboxID] != "sbx_fork" || obj["spec"].(map[string]any)["shutdownPolicy"] != "Delete" {
			t.Fatalf("fork metadata %v, spec %v", md, obj["spec"])
		}
		if !sameJSON(tmpl["spec"], f.object("sandboxes/dly-sbx-src")["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"]) {
			t.Fatalf("the fork's pod spec differs from its source's")
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
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_gone", Installation: "inst_1", FromSnapshot: snap.Ref}); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("fork of a deleted snapshot: %v", err)
		}
	}
}

func TestForkThatGKEDidNotRestoreFails(t *testing.T) {
	f := newFakeSnapshots(t)
	a := f.adapter(t, snapCfg())
	ctx := context.Background()
	src, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_src", Installation: "inst_1"})
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
	_, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_fork", Installation: "inst_1", FromSnapshot: snap.Ref})
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
	}
	if err := a.Delete(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if s, tr := f.count(); s != 0 || tr != 0 {
		t.Fatalf("delete left %d pause snapshots and %d triggers", s, tr)
	}
}

func TestResumeThatGKEDidNotRestoreStaysPaused(t *testing.T) {
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
}
