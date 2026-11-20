//go:build experimental_adapters

package agentsandbox

// GKE Pod Snapshots (podsnapshot.gke.io/v1) for Sandboxes that run under
// gVisor on GKE: memory snapshots, fork from a snapshot, and pause/resume
// that writes the sandbox out and deletes its pod.
//
// GKE checkpoints a pod when a PodSnapshotManualTrigger names it, and
// restores every new pod that matches a PodSnapshotPolicy: from the snapshot
// its annotation podsnapshot.gke.io/ps-name names, or else from the newest
// snapshot with a matching pod spec in its group. Three properties of that
// design shape this file:
//
//   - Any new pod is restored. Without grouping, a fresh sandbox could start
//     from another one's memory, so the pool refuses a policy that does not
//     select on, and group by, the label snapshotGroup. Every sandbox's pod
//     carries a value of its own: a fork starts in its source's group, which
//     its restore needs, and moves to its own group as soon as the restore is
//     verified (agent-sandbox's controller patches template label changes
//     onto the running pod; it does not recreate it). A pod GKE recreates
//     later (a node drain) therefore comes back from the sandbox's own newest
//     checkpoint, never from another sandbox's.
//   - A pod that cannot be restored (another machine series, a different pod
//     spec, a snapshot that is not ready) starts fresh, silently. Before each
//     checkpoint the adapter creates a marker file named after it in the
//     sandbox and looks for it after a restore. Each checkpoint has its own
//     file, so concurrent checkpoints cannot overwrite each other's.
//   - Restores come from the pod template, so stop/start would restore the
//     newest checkpoint over a newer volume. With snapshots, the pool offers
//     pause/resume instead of stop/start.
//
// With a warm pool, sandboxes are SandboxClaims on already running pods. A
// running pod cannot be restored, so such a pool offers pause/resume only, no
// snapshots or fork. The claim controller rewrites the claimed Sandbox's pod
// template from the SandboxTemplate and the claim on every pass, so the
// snapshot group and the checkpoint to restore go into the claim's
// additionalPodMetadata, and the adapter's annotations onto the claim; only
// the operating mode is set on the Sandbox itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const (
	snapGroup   = "podsnapshot.gke.io"
	snapVersion = "v1"

	// snapshotGroup is the pod label the PodSnapshotPolicy must select on
	// (operator Exists) and group by.
	snapshotGroup = "differently.dev/snapshot-group"
	// psName is GKE's pod annotation naming the snapshot to restore.
	psName = "podsnapshot.gke.io/ps-name"
	// markerPrefix plus a checkpoint's token is the file created in the
	// sandbox just before that checkpoint.
	markerPrefix = "/tmp/.differently-snapshot-"
	// maxRestoreFailures is how many resumes may find the pause checkpoint
	// not restored before the sandbox is reported failed.
	maxRestoreFailures = 3
	// collectAfter is how old an orphaned pause checkpoint must be before
	// List deletes it, so a sandbox created during a long List keeps its own.
	collectAfter = 15 * time.Minute

	annoToken    = "differently.dev/restore-token"    // on a Sandbox: the checkpoint its pod must come from
	annoPaused   = "differently.dev/paused"           // on a Sandbox: the trigger of the pause it is in
	annoFailures = "differently.dev/restore-failures" // on a Sandbox: resumes that were not restored
	annoForking  = "differently.dev/fork-pending"     // on a Sandbox: a fork not yet verified and regrouped
	annoTemplate = "differently.dev/sandbox-spec"     // on a trigger: the source Sandbox's spec, for forks
	annoGroup    = "differently.dev/snapshot-group"   // on a trigger: the source's group at checkpoint
	labelPurpose = "differently.dev/snapshot-purpose" // on a trigger: snapshot or pause
	labelSource  = "differently.dev/snapshot-source"  // on a trigger: the Sandbox it checkpointed
)

// SnapshotFeatures are added when the pool sets config.podSnapshotPolicy.
var SnapshotFeatures = []string{
	adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork,
	adapter.FeaturePauseResume, adapter.FeaturePauseCheckpoint,
}

// WarmSnapshotFeatures are added instead when the pool also sets
// config.warmPool.
var WarmSnapshotFeatures = []string{adapter.FeaturePauseResume, adapter.FeaturePauseCheckpoint}

func (a *Adapter) snapPath(plural string) string {
	return "/apis/" + snapGroup + "/" + snapVersion + "/namespaces/" + url.PathEscape(a.opts.Namespace) + "/" + plural
}

func (a *Adapter) triggers() string { return a.snapPath("podsnapshotmanualtriggers") }

func (a *Adapter) snapshots() bool { return a.opts.PodSnapshotPolicy != "" }

func (a *Adapter) warm() bool { return a.opts.WarmPool != "" }

type expression struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
}

// policy is the part of a PodSnapshotPolicy the adapter checks.
type policy struct {
	Spec struct {
		Selector struct {
			MatchExpressions []expression `json:"matchExpressions"`
		} `json:"selector"`
		SnapshotScope string `json:"snapshotScope"`
		TriggerConfig struct {
			Type           string `json:"type"`
			PostCheckpoint string `json:"postCheckpoint"`
		} `json:"triggerConfig"`
		SnapshotGroupingRules struct {
			GroupByLabelValue struct {
				Labels               []string `json:"labels"`
				GroupRetentionPolicy struct {
					MaxSnapshotCountPerGroup int `json:"maxSnapshotCountPerGroup"`
				} `json:"groupRetentionPolicy"`
			} `json:"groupByLabelValue"`
		} `json:"snapshotGroupingRules"`
	} `json:"spec"`
	Status struct {
		Conditions []condition `json:"conditions"`
	} `json:"status"`
}

// check refuses a policy under which sandboxes could restore each other's
// snapshots, or under which the adapter's own snapshots could vanish.
func (p policy) check(name string) error {
	bad := func(format string, args ...any) error {
		return adapter.Unavailable(fmt.Errorf("agent-sandbox: PodSnapshotPolicy %s: "+format, append([]any{name}, args...)...))
	}
	if p.Spec.TriggerConfig.Type != "manual" {
		return bad("triggerConfig.type must be manual, not %q", p.Spec.TriggerConfig.Type)
	}
	if sc := p.Spec.SnapshotScope; sc != "" && sc != "whole-pod" {
		// rootfs-only keeps files but not memory or processes, which the pool
		// advertises.
		return bad("snapshotScope must be whole-pod (the default), not %q", sc)
	}
	if pc := p.Spec.TriggerConfig.PostCheckpoint; pc != "" && pc != "resume" {
		return bad("triggerConfig.postCheckpoint must be resume (the default), not %q", pc)
	}
	if !slices.Contains(p.Spec.Selector.MatchExpressions, expression{Key: snapshotGroup, Operator: "Exists"}) {
		return bad("selector.matchExpressions must include {key: %s, operator: Exists}", snapshotGroup)
	}
	g := p.Spec.SnapshotGroupingRules.GroupByLabelValue
	if !slices.Contains(g.Labels, snapshotGroup) {
		return bad("snapshotGroupingRules.groupByLabelValue.labels must include %s, or a new sandbox could be restored from another sandbox's snapshot", snapshotGroup)
	}
	if g.GroupRetentionPolicy.MaxSnapshotCountPerGroup != 0 {
		return bad("groupRetentionPolicy.maxSnapshotCountPerGroup must be 0 (no limit): GKE would delete snapshots that Differently still lists")
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status != "True" {
			return bad("not ready: %s %s", c.Reason, c.Message)
		}
	}
	return nil
}

func (a *Adapter) checkPolicy(ctx context.Context) error {
	var p policy
	err := a.k.do(ctx, http.MethodGet, a.snapPath("podsnapshotpolicies")+"/"+url.PathEscape(a.opts.PodSnapshotPolicy), "", nil, &p)
	if api.IsCode(err, api.CodeNotFound) {
		return adapter.Unavailable(fmt.Errorf("agent-sandbox: PodSnapshotPolicy %s not found in namespace %s (is GKE Pod Snapshots enabled?)", a.opts.PodSnapshotPolicy, a.opts.Namespace))
	}
	if err != nil {
		return err
	}
	return p.check(a.opts.PodSnapshotPolicy)
}

// checkWarmRuntime refuses a warm pool whose pods GKE cannot checkpoint: Pod
// Snapshots need gVisor, which a pod gets from its RuntimeClass.
func (a *Adapter) checkWarmRuntime(ctx context.Context, template string) error {
	if template == "" {
		return adapter.Unavailable(fmt.Errorf("agent-sandbox: SandboxWarmPool %s names no sandboxTemplateRef", a.opts.WarmPool))
	}
	var t struct {
		Spec struct {
			PodTemplate struct {
				Spec struct {
					RuntimeClassName string `json:"runtimeClassName"`
				} `json:"spec"`
			} `json:"podTemplate"`
		} `json:"spec"`
	}
	err := a.k.do(ctx, http.MethodGet, a.path(extensions, "sandboxtemplates")+"/"+url.PathEscape(template), "", nil, &t)
	if api.IsCode(err, api.CodeNotFound) {
		return adapter.Unavailable(fmt.Errorf("agent-sandbox: SandboxTemplate %s of warm pool %s not found", template, a.opts.WarmPool))
	}
	if err != nil {
		return err
	}
	if t.Spec.PodTemplate.Spec.RuntimeClassName == "" {
		return adapter.Unavailable(fmt.Errorf("agent-sandbox: GKE Pod Snapshots need gVisor, but SandboxTemplate %s of warm pool %s sets no runtimeClassName", template, a.opts.WarmPool))
	}
	return nil
}

// trigger is a PodSnapshotManualTrigger: the fields the adapter reads.
type trigger struct {
	Metadata struct {
		Name              string            `json:"name"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		Conditions []condition `json:"conditions"`
		// A string in GKE's CRD reference, an object with a name in its
		// agent-sandbox guide: both are read.
		SnapshotCreated json.RawMessage `json:"snapshotCreated"`
	} `json:"status"`
}

func (t trigger) created() string {
	var s string
	if json.Unmarshal(t.Status.SnapshotCreated, &s) == nil {
		return s
	}
	var o struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(t.Status.SnapshotCreated, &o)
	return o.Name
}

// failed reports a trigger whose checkpoint ended without a snapshot. GKE
// does not document its failure reasons; one that names neither a failure
// nor an error is waited out until config.snapshotTimeout.
func (t trigger) failed() (string, bool) {
	for _, c := range t.Status.Conditions {
		if c.Status == "False" && (strings.Contains(c.Reason, "Fail") || strings.Contains(c.Reason, "Error")) {
			return strings.TrimSpace(c.Reason + " " + c.Message), true
		}
	}
	return "", false
}

// newToken is unique per checkpoint and safe as part of a Kubernetes name.
func newToken() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

func (a *Adapter) writeMarker(ctx context.Context, ref, token string) error {
	if _, err := a.shell(ctx, ref, `: > "$1"`, nil, markerPrefix+token); err != nil {
		return fmt.Errorf("agent-sandbox: creating the snapshot marker %s in %s: %w", markerPrefix+token, ref, err)
	}
	return nil
}

// restored reports whether ref's pod holds the marker of the checkpoint
// with this token, that is, whether GKE restored it from that checkpoint.
func (a *Adapter) restored(ctx context.Context, ref, token string) (bool, error) {
	code, err := a.exec(ctx, ref, []string{"test", "-e", markerPrefix + token}, nil, func(string, []byte) error { return nil })
	return err == nil && code == 0, err
}

// checkpoint creates the marker, triggers a snapshot of ref's pod and waits
// for the PodSnapshot to be ready. It returns the trigger's and the
// snapshot's names and the checkpoint's token.
func (a *Adapter) checkpoint(ctx context.Context, ref, installation, purpose string, annotations map[string]string) (trig, snap, token string, err error) {
	pod, err := a.sandbox(ctx, ref) // the pod has the Sandbox's name
	if err != nil {
		return "", "", "", err
	}
	token = newToken()
	if err = a.writeMarker(ctx, ref, token); err != nil {
		return "", "", "", err
	}
	trig = ref + "-" + token
	annotations[annoToken] = token
	body := map[string]any{
		"apiVersion": snapGroup + "/" + snapVersion, "kind": "PodSnapshotManualTrigger",
		"metadata": map[string]any{
			"name":        trig,
			"labels":      map[string]string{labelPurpose: purpose, labelSource: ref, adapter.LabelInstallation: installation},
			"annotations": annotations,
		},
		"spec": map[string]string{"targetPod": pod},
	}
	if err = a.k.do(ctx, http.MethodPost, a.triggers(), "", body, nil); err != nil {
		return "", "", "", err
	}
	err = httpx.Poll(ctx, time.Second, a.opts.SnapshotTimeout, func() (bool, error) {
		var t trigger
		if err := a.k.do(ctx, http.MethodGet, a.triggers()+"/"+url.PathEscape(trig), "", nil, &t); err != nil {
			return false, err
		}
		if msg, bad := t.failed(); bad {
			return false, adapter.SandboxFailed(fmt.Errorf("agent-sandbox: checkpoint of %s failed: %s", ref, msg))
		}
		if snap = t.created(); snap == "" {
			return false, nil
		}
		return a.snapshotReady(ctx, snap)
	})
	if err != nil {
		// A PodSnapshot GKE finishes after its trigger is gone is not tracked;
		// it sits in ref's own group, so it can only ever restore ref.
		_ = a.deleteCheckpoint(context.WithoutCancel(ctx), trig, snap)
		return "", "", "", timedOut(err, fmt.Sprintf("the snapshot of %s was not ready within %s", ref, a.opts.SnapshotTimeout))
	}
	return trig, snap, token, nil
}

// timedOut turns a poll's timeout into sandbox_failed: the cluster is fine,
// this one sandbox's operation is not.
func timedOut(err error, msg string) error {
	if api.IsCode(err, api.CodeProviderUnavailable) && strings.Contains(err.Error(), "timed out") {
		return adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s", msg))
	}
	return err
}

func (a *Adapter) snapshotReady(ctx context.Context, snap string) (bool, error) {
	var ps object
	if err := a.k.do(ctx, http.MethodGet, a.snapPath("podsnapshots")+"/"+url.PathEscape(snap), "", nil, &ps); err != nil {
		if api.IsCode(err, api.CodeNotFound) {
			return false, nil // the trigger names it before it exists
		}
		return false, err
	}
	if c, ok := ps.cond("Ready"); ok && c.Status == "False" && (strings.Contains(c.Reason, "Fail") || strings.Contains(c.Reason, "Error")) {
		return false, adapter.SandboxFailed(fmt.Errorf("agent-sandbox: PodSnapshot %s failed: %s %s", snap, c.Reason, c.Message))
	}
	return ps.is("Ready"), nil
}

// deleteCheckpoint removes a snapshot (GKE deletes its files) and its trigger.
func (a *Adapter) deleteCheckpoint(ctx context.Context, trig, snap string) error {
	var errs []error
	if snap != "" {
		if err := a.k.do(ctx, http.MethodDelete, a.snapPath("podsnapshots")+"/"+url.PathEscape(snap), "", nil, nil); err != nil && !api.IsCode(err, api.CodeNotFound) {
			errs = append(errs, err)
		}
	}
	if trig != "" {
		if err := a.k.do(ctx, http.MethodDelete, a.triggers()+"/"+url.PathEscape(trig), "", nil, nil); err != nil && !api.IsCode(err, api.CodeNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// snapshotRef joins a snapshot's and its trigger's names; '/' is never part
// of a Kubernetes name.
func snapshotRef(snap, trig string) string { return snap + "/" + trig }

func parseSnapshotRef(ref string) (snap, trig string, err error) {
	snap, trig, ok := strings.Cut(ref, "/")
	if !ok || snap == "" || trig == "" {
		return "", "", api.Errorf(api.CodeInvalidRequest, "agent-sandbox: %q is not a snapshot of this adapter", ref)
	}
	return snap, trig, nil
}

func (a *Adapter) needSnapshots(feature string) error {
	if !a.snapshots() || (a.warm() && !slices.Contains(WarmSnapshotFeatures, feature)) {
		return adapter.Unsupported(feature)
	}
	return nil
}

// sandboxSpec is what a fork copies from its source: the pod spec, the
// volume claim templates and the Service flag. GKE restores a pod only when
// its spec matches the snapshot's, so a fork does not take the request's
// image or env, and its size must be its source's.
type sandboxSpec struct {
	PodSpec              json.RawMessage `json:"podSpec"`
	VolumeClaimTemplates json.RawMessage `json:"volumeClaimTemplates,omitempty"`
	Service              json.RawMessage `json:"service,omitempty"`
}

// readSpec returns ref's spec, its pod's snapshot group and its installation.
func (a *Adapter) readSpec(ctx context.Context, ref string) (sandboxSpec, string, string, error) {
	var s struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			PodTemplate struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
				Spec json.RawMessage `json:"spec"`
			} `json:"podTemplate"`
			VolumeClaimTemplates json.RawMessage `json:"volumeClaimTemplates"`
			Service              json.RawMessage `json:"service"`
		} `json:"spec"`
	}
	if err := a.k.do(ctx, http.MethodGet, a.sandboxes()+"/"+url.PathEscape(ref), "", nil, &s); err != nil {
		return sandboxSpec{}, "", "", err
	}
	group := s.Spec.PodTemplate.Metadata.Labels[snapshotGroup]
	if group == "" {
		return sandboxSpec{}, "", "", api.Errorf(api.CodeInvalidState, "agent-sandbox: %s was created without snapshots enabled (no %s label)", ref, snapshotGroup)
	}
	return sandboxSpec{PodSpec: s.Spec.PodTemplate.Spec, VolumeClaimTemplates: s.Spec.VolumeClaimTemplates, Service: s.Spec.Service}, group, s.Metadata.Labels[adapter.LabelInstallation], nil
}

// Snapshot checkpoints the sandbox's whole pod: memory, processes and its
// root filesystem. Volumes are not captured. The sandbox keeps running.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if err := a.needSnapshots(adapter.FeatureSnapshotMem); err != nil {
		return adapter.Snapshot{}, err
	}
	spec, group, installation, err := a.readSpec(ctx, ref)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	trig, snap, _, err := a.checkpoint(ctx, ref, installation, "snapshot", map[string]string{annoTemplate: string(raw), annoGroup: group})
	if err != nil {
		return adapter.Snapshot{}, err
	}
	// A pod snapshot always holds memory, whatever kind was asked for.
	return adapter.Snapshot{Ref: snapshotRef(snap, trig), Kind: "memory"}, nil
}

// DeleteSnapshot removes the snapshot and its files.
func (a *Adapter) DeleteSnapshot(ctx context.Context, snapshotRef string) error {
	snap, trig, err := parseSnapshotRef(snapshotRef)
	if err != nil {
		return err
	}
	return a.deleteCheckpoint(ctx, trig, snap)
}

// sizeOf reads the cpu and memory limits the adapter's own manifest sets.
func sizeOf(podSpec json.RawMessage) (cpu, memory string) {
	var s struct {
		Containers []struct {
			Resources struct {
				Limits map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	}
	if json.Unmarshal(podSpec, &s) != nil || len(s.Containers) == 0 {
		return "", ""
	}
	return s.Containers[0].Resources.Limits["cpu"], s.Containers[0].Resources.Limits["memory"]
}

// forkManifest is a Sandbox restored from a snapshot: the source's spec, in
// the source's snapshot group (until adopt moves it), naming the snapshot.
func (a *Adapter) forkManifest(ctx context.Context, req adapter.CreateRequest) (map[string]any, string, error) {
	snap, trig, err := parseSnapshotRef(req.FromSnapshot)
	if err != nil {
		return nil, "", err
	}
	var t trigger
	if err := a.k.do(ctx, http.MethodGet, a.triggers()+"/"+url.PathEscape(trig), "", nil, &t); err != nil {
		return nil, "", err
	}
	if t.created() != snap || t.Metadata.Annotations[annoTemplate] == "" {
		return nil, "", api.Errorf(api.CodeInvalidRequest, "agent-sandbox: snapshot %s cannot be forked", req.FromSnapshot)
	}
	var spec sandboxSpec
	if err := json.Unmarshal([]byte(t.Metadata.Annotations[annoTemplate]), &spec); err != nil {
		return nil, "", fmt.Errorf("agent-sandbox: snapshot %s: %w", snap, err)
	}
	// The fork runs at its source's size, so it must be asked for at that
	// size, or the pool's capacity and the tenant's quota would count less
	// than it uses.
	cpu, mem := sizeOf(spec.PodSpec)
	wantCPU, wantMem := strconv.Itoa(max(req.Resources.CPU, 1)), strconv.Itoa(max(req.Resources.MemoryMiB, 128))+"Mi"
	if cpu != wantCPU || mem != wantMem {
		return nil, "", api.Errorf(api.CodeInvalidRequest, "agent-sandbox: a fork runs at its source's size: ask for cpu %s and memory %s, not cpu %s and memory %s", cpu, mem, wantCPU, wantMem)
	}
	if ok, err := a.snapshotReady(ctx, snap); err != nil || !ok {
		if err == nil {
			err = api.Errorf(api.CodeInvalidState, "agent-sandbox: snapshot %s is not ready", snap)
		}
		return nil, "", err
	}
	token := t.Metadata.Annotations[annoToken]
	labels := adapter.OwnershipLabels(req)
	podLabels := map[string]string{snapshotGroup: t.Metadata.Annotations[annoGroup]}
	for k, v := range labels {
		podLabels[k] = v
	}
	sbSpec := map[string]any{"podTemplate": map[string]any{
		"metadata": map[string]any{"labels": podLabels, "annotations": map[string]string{psName: snap}},
		"spec":     spec.PodSpec,
	}}
	if len(spec.VolumeClaimTemplates) > 0 && string(spec.VolumeClaimTemplates) != "null" {
		sbSpec["volumeClaimTemplates"] = spec.VolumeClaimTemplates
	}
	if len(spec.Service) > 0 && string(spec.Service) != "null" {
		sbSpec["service"] = spec.Service
	}
	if req.TTLSeconds > 0 {
		sbSpec["shutdownTime"] = time.Now().Add(time.Duration(req.TTLSeconds) * time.Second).UTC().Format(time.RFC3339)
		sbSpec["shutdownPolicy"] = "Delete"
	}
	return map[string]any{
		"apiVersion": group + "/" + version, "kind": "Sandbox",
		"metadata": map[string]any{"name": name(req.ID), "labels": labels, "annotations": map[string]string{annoToken: token, annoForking: "true"}},
		"spec":     sbSpec,
	}, token, nil
}

// verifyRestore checks that ref's pod came from the checkpoint with token.
func (a *Adapter) verifyRestore(ctx context.Context, ref, token string) error {
	ok, err := a.restored(ctx, ref, token)
	if err != nil {
		return err
	}
	if !ok {
		return adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s started without its snapshot (GKE restores only onto the same machine series, with the same pod spec, from a ready snapshot); its memory and files were not restored", ref))
	}
	return nil
}

// adopt moves a restored fork into its own snapshot group and drops the
// snapshot it was restored from, then waits until the running pod carries
// the new group, so none of the fork's checkpoints land in its source's
// group. The controller patches the pod's labels; it does not recreate it.
func (a *Adapter) adopt(ctx context.Context, ref string) error {
	err := a.patchSandbox(ctx, ref, map[string]any{"spec": map[string]any{"podTemplate": map[string]any{"metadata": map[string]any{
		"labels":      map[string]string{snapshotGroup: ref},
		"annotations": map[string]any{psName: nil},
	}}}})
	if err != nil {
		return err
	}
	if err := a.waitGroup(ctx, ref, ref); err != nil {
		return err
	}
	return a.patchSandbox(ctx, ref, map[string]any{"metadata": map[string]any{"annotations": map[string]any{annoForking: nil}}})
}

// waitGroup waits until the running pod carries ref as its snapshot group,
// which GKE reads from the pod, not from the template.
func (a *Adapter) waitGroup(ctx context.Context, ref, pod string) error {
	err := httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		var p struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		err := a.k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(a.opts.Namespace)+"/pods/"+url.PathEscape(pod), "", nil, &p)
		return err == nil && p.Metadata.Labels[snapshotGroup] == ref, err
	})
	if err != nil {
		return timedOut(err, fmt.Sprintf("the pod of %s did not take its own snapshot group within %s", ref, a.opts.ReadyTimeout))
	}
	return nil
}

// finishFork completes a fork whose Create was interrupted (the control
// plane restarted between creating it and moving it to its own group), so
// the reconciler, which adopts what List returns, never adopts a fork still
// in its source's group. It reports whether the fork may be listed. A fork
// that was not restored is deleted: the scheduler then creates it again.
func (a *Adapter) finishFork(ctx context.Context, s object) bool {
	ref := s.Metadata.Name
	if !s.is("Ready") {
		return false
	}
	ok, err := a.restored(ctx, ref, s.Metadata.Annotations[annoToken])
	switch {
	case err != nil:
		return false // try again on the next List
	case !ok:
		_ = a.Delete(ctx, ref)
		return false
	}
	return a.adopt(ctx, ref) == nil
}

func (a *Adapter) patchSandbox(ctx context.Context, ref string, patch map[string]any) error {
	return a.k.do(ctx, http.MethodPatch, a.sandboxes()+"/"+url.PathEscape(ref), "application/merge-patch+json", patch, nil)
}

// failures is how many resumes found the pause checkpoint not restored.
func (s object) failures() int {
	n, _ := strconv.Atoi(s.Metadata.Annotations[annoFailures])
	return n
}

// record returns ref's own object, which holds the adapter's annotations,
// and the Sandbox that runs it: the same object without a warm pool, the
// claimed Sandbox with one.
func (a *Adapter) record(ctx context.Context, ref string) (rec, sb object, err error) {
	if rec, err = a.get(ctx, a.owned(), ref); err != nil || !a.warm() {
		return rec, rec, err
	}
	sb, err = a.claimed(ctx, ref)
	return rec, sb, err
}

// claimed returns the Sandbox claim ref adopted. When the Sandbox it was
// bound to is gone, the claim controller adopts another, so the binding is
// dropped and read again from the claim once.
func (a *Adapter) claimed(ctx context.Context, ref string) (object, error) {
	for attempt := 0; ; attempt++ {
		name, err := a.sandbox(ctx, ref)
		if err != nil {
			return object{}, err
		}
		sb, err := a.get(ctx, a.sandboxes(), name)
		if !api.IsCode(err, api.CodeNotFound) || attempt > 0 {
			return sb, err
		}
		a.unbind(ref)
	}
}

func (a *Adapter) unbind(ref string) {
	a.mu.Lock()
	delete(a.bound, ref)
	a.mu.Unlock()
}

// update writes, each when given, ref's own annotations, the annotations of
// its pod template and its Sandbox's operating mode. Without a warm pool
// that is one patch of one Sandbox. With one, the annotations go on the
// claim, the pod's through its additionalPodMetadata (the claim controller
// copies them onto the Sandbox), and the mode on the claimed Sandbox: last
// when entering a pause, so a sandbox is never suspended without the claim
// saying so; first otherwise (undoing a pause, suspending an unverified
// resume), so a failed claim patch cannot leave the mode undone.
func (a *Adapter) update(ctx context.Context, ref, sb string, annotations, podAnnotations map[string]any, mode string) error {
	patch, spec := map[string]any{}, map[string]any{}
	if annotations != nil {
		patch["metadata"] = map[string]any{"annotations": annotations}
	}
	if !a.warm() {
		if mode != "" {
			spec["operatingMode"] = mode
		}
		if podAnnotations != nil {
			spec["podTemplate"] = map[string]any{"metadata": map[string]any{"annotations": podAnnotations}}
		}
		if len(spec) > 0 {
			patch["spec"] = spec
		}
		return a.patchSandbox(ctx, ref, patch)
	}
	if podAnnotations != nil {
		patch["spec"] = map[string]any{"additionalPodMetadata": map[string]any{"annotations": podAnnotations}}
	}
	setMode := func() error {
		if mode == "" {
			return nil
		}
		return a.patch(ctx, a.sandboxes(), sb, map[string]any{"operatingMode": mode})
	}
	pausing := annotations != nil && annotations[annoPaused] != nil
	if !pausing {
		if err := setMode(); err != nil {
			return err
		}
	}
	if len(patch) > 0 {
		if err := a.k.do(ctx, http.MethodPatch, a.owned()+"/"+url.PathEscape(ref), "application/merge-patch+json", patch, nil); err != nil {
			return err
		}
	}
	if pausing {
		return setMode()
	}
	return nil
}

// instOf reports s. A claim whose sandbox is pausing, paused, resuming or
// suspended takes its state from the claimed Sandbox: the claim's own
// conditions do not describe a suspended Sandbox. A Sandbox that cannot be
// read reports that one sandbox pending (which the reconciler leaves alone)
// rather than failing a whole List.
func (a *Adapter) instOf(ctx context.Context, s object) (adapter.Instance, error) {
	c, _ := s.cond("Ready")
	if !a.warm() || (s.Metadata.Annotations[annoPaused] == "" && c.Reason != "SandboxSuspended") {
		return s.inst(), nil
	}
	sb, err := a.claimed(ctx, s.Metadata.Name)
	if err != nil {
		in := s.inst()
		in.State, in.Message = adapter.StatePending, "its Sandbox cannot be read: "+err.Error()
		return in, nil
	}
	sb.Metadata.Name, sb.Metadata.Labels, sb.Metadata.Annotations = s.Metadata.Name, s.Metadata.Labels, s.Metadata.Annotations
	return sb.inst(), nil
}

// Pause checkpoints the sandbox and suspends it: the pod is deleted, so the
// sandbox holds no node resources while paused. Resume restores that
// checkpoint. Only the newest pause checkpoint is kept.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if err := a.needSnapshots(adapter.FeaturePauseResume); err != nil {
		return err
	}
	rec, sb, err := a.record(ctx, ref)
	if err != nil {
		return err
	}
	if rec.Metadata.Annotations[annoPaused] != "" && sb.Spec.OperatingMode == "Suspended" {
		return nil // already paused
	}
	if sb.Spec.OperatingMode == "Suspended" {
		return api.Errorf(api.CodeInvalidState, "agent-sandbox: %s is stopped", ref)
	}
	sbName := sb.Metadata.Name
	if a.warm() {
		// A claimed warm pod joins its group a moment after the claim; GKE
		// checkpoints only a pod its policy selects.
		if err := a.waitGroup(ctx, ref, sbName); err != nil {
			return err
		}
	}
	trig, snap, token, err := a.checkpoint(ctx, ref, rec.Metadata.Labels[adapter.LabelInstallation], "pause", map[string]string{})
	if err != nil {
		return err
	}
	// The pod the controller creates on resume restores this snapshot.
	err = a.update(ctx, ref, sbName, map[string]any{annoPaused: trig, annoToken: token, annoFailures: nil}, map[string]any{psName: snap}, "Suspended")
	if err != nil {
		bg := context.WithoutCancel(ctx)
		if a.warm() {
			// The claim may already name the checkpoint about to be deleted,
			// and the suspend may have landed with its answer lost: set the
			// Sandbox running first, then clear the claim.
			if rerr := a.update(bg, ref, sbName, map[string]any{annoPaused: nil}, map[string]any{psName: nil}, "Running"); rerr != nil {
				// Something may still need the checkpoint: keep it, as the
				// orphan collection does for a live sandbox.
				return err
			}
		}
		_ = a.deleteCheckpoint(bg, trig, snap)
		return err
	}
	// Suspended, and, with a warm pool, the claim controller has copied the
	// checkpoint's name onto the Sandbox's pod template.
	err = httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		_, s, err := a.record(ctx, ref)
		return err == nil && s.is("Suspended") && s.Spec.PodTemplate.Metadata.Annotations[psName] == snap, err
	})
	if err != nil {
		if ctx.Err() != nil {
			// The caller gave up, typically because the reconciler saw the
			// pause and recorded it: the pause stands.
			return err
		}
		// The suspend did not finish. The scheduler keeps the sandbox running
		// when a pause fails, so running it must be. A pod recreated meanwhile
		// restores this checkpoint, the newest state there is.
		rerr := a.update(context.WithoutCancel(ctx), ref, sbName, map[string]any{annoPaused: nil}, nil, "Running")
		if rerr != nil {
			return fmt.Errorf("agent-sandbox: %s was not suspended within %s, and setting it running again failed: %w", ref, a.opts.ReadyTimeout, rerr)
		}
		return timedOut(err, fmt.Sprintf("%s was not suspended within %s; it was set running again", ref, a.opts.ReadyTimeout))
	}
	a.dropCheckpoints(ctx, map[string]string{labelPurpose: "pause", labelSource: ref}, func(t trigger) bool { return t.Metadata.Name != trig })
	return nil
}

// Resume restarts the pod, which GKE restores from the pause checkpoint, and
// checks that it did. If it did not, the sandbox is suspended again, so it
// never runs on without its state, and a later resume tries again (GKE may
// place it on a node it can restore onto). After maxRestoreFailures such
// resumes the sandbox reports an error, which fails it; its pause checkpoint
// is then kept, relabelled unrestored, for recovery by hand.
func (a *Adapter) Resume(ctx context.Context, ref string) error {
	if err := a.needSnapshots(adapter.FeaturePauseResume); err != nil {
		return err
	}
	rec, sb, err := a.record(ctx, ref)
	if err != nil {
		return err
	}
	if rec.Metadata.Annotations[annoPaused] == "" {
		if sb.Spec.OperatingMode == "Suspended" {
			return api.Errorf(api.CodeInvalidState, "agent-sandbox: %s is stopped, not paused", ref)
		}
		return nil // running
	}
	if n := rec.failures(); n >= maxRestoreFailures {
		return adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s could not be restored from its pause checkpoint in %d resumes", ref, n))
	}
	token, sbName := rec.Metadata.Annotations[annoToken], sb.Metadata.Name
	if sb.Spec.OperatingMode == "Suspended" {
		if a.warm() {
			// The pod must not start before the claim controller has copied
			// the checkpoint's name onto the Sandbox's pod template (a pause
			// whose caller gave up may not have waited for it).
			want := rec.Spec.AdditionalPodMetadata.Annotations[psName]
			err := httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
				s, err := a.get(ctx, a.sandboxes(), sbName)
				return err == nil && want != "" && s.Spec.PodTemplate.Metadata.Annotations[psName] == want, err
			})
			if err != nil {
				return timedOut(err, fmt.Sprintf("the Sandbox of %s did not name its pause checkpoint within %s", ref, a.opts.ReadyTimeout))
			}
		}
		if err := a.patch(ctx, a.sandboxes(), sbName, map[string]any{"operatingMode": "Running"}); err != nil {
			return err
		}
	}
	if _, err := a.waitReady(ctx, ref); err != nil {
		return err
	}
	ok, err := a.restored(ctx, ref, token)
	if err != nil || !ok {
		// Suspend it again either way: it must not run on unverified. Only a
		// pod that answered, without the marker, counts as not restored.
		var annotations map[string]any
		if err == nil {
			annotations = map[string]any{annoFailures: strconv.Itoa(rec.failures() + 1)}
			err = adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s started without its pause checkpoint (GKE restores only onto the same machine series, from a ready snapshot); it was suspended again", ref))
		}
		_ = a.update(context.WithoutCancel(ctx), ref, sbName, annotations, nil, "Suspended")
		return err
	}
	// The checkpoint stays until the next pause or the delete: GKE may still
	// be streaming memory into the pod from it. The pod template stops naming
	// it, so a pod GKE recreates later restores the sandbox's newest
	// checkpoint instead of rolling back to this one.
	return a.update(ctx, ref, sbName, map[string]any{annoPaused: nil, annoFailures: nil}, map[string]any{psName: nil}, "")
}

// dropCheckpoints deletes the checkpoints whose triggers carry labels and
// that drop selects. Failures are left for the next call: a leftover costs
// storage only, and can only restore the sandbox it came from.
func (a *Adapter) dropCheckpoints(ctx context.Context, labels map[string]string, drop func(trigger) bool) {
	sel := make([]string, 0, len(labels))
	for k, v := range labels {
		sel = append(sel, k+"="+v)
	}
	slices.Sort(sel)
	var list struct {
		Items []trigger `json:"items"`
	}
	if err := a.k.do(ctx, http.MethodGet, a.triggers()+"?"+url.Values{"labelSelector": {strings.Join(sel, ",")}}.Encode(), "", nil, &list); err != nil {
		return
	}
	for _, t := range list.Items {
		if drop(t) {
			_ = a.deleteCheckpoint(ctx, t.Metadata.Name, t.created())
		}
	}
}

// collectPauses deletes this installation's pause checkpoints whose sandbox
// no longer exists: deleted behind Differently's back (the TTL backstop), or
// a delete whose clean-up failed. live holds the installation's Sandboxes.
func (a *Adapter) collectPauses(ctx context.Context, installation string, live map[string]bool) {
	a.dropCheckpoints(ctx, map[string]string{labelPurpose: "pause", adapter.LabelInstallation: installation}, func(t trigger) bool {
		return !live[t.Metadata.Labels[labelSource]] && time.Since(t.createdAt()) > collectAfter
	})
}

// createdAt is the trigger's creation time, or now when the API server did
// not say (so it is not collected).
func (t trigger) createdAt() time.Time {
	if t.Metadata.CreationTimestamp.IsZero() {
		return time.Now()
	}
	return t.Metadata.CreationTimestamp
}

// keepUnrestored relabels ref's pause checkpoints so that nothing deletes
// them: the sandbox is being deleted after GKE never restored it, and the
// checkpoint is the only copy of its state.
func (a *Adapter) keepUnrestored(ctx context.Context, ref string) {
	var list struct {
		Items []trigger `json:"items"`
	}
	q := url.Values{"labelSelector": {labelPurpose + "=pause," + labelSource + "=" + ref}}
	if err := a.k.do(ctx, http.MethodGet, a.triggers()+"?"+q.Encode(), "", nil, &list); err != nil {
		return
	}
	for _, t := range list.Items {
		_ = a.k.do(ctx, http.MethodPatch, a.triggers()+"/"+url.PathEscape(t.Metadata.Name), "application/merge-patch+json",
			map[string]any{"metadata": map[string]any{"labels": map[string]string{labelPurpose: "unrestored"}}}, nil)
	}
}

var (
	_ adapter.Pauser      = (*Adapter)(nil)
	_ adapter.Snapshotter = (*Adapter)(nil)
)
