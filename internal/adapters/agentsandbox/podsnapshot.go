//go:build experimental_adapters

package agentsandbox

// GKE Pod Snapshots (podsnapshot.gke.io/v1) for Sandboxes that run under
// gVisor on GKE: memory snapshots, fork from a snapshot, and pause/resume
// that writes the sandbox out and deletes its pod.
//
// GKE checkpoints a pod when a PodSnapshotManualTrigger names it, and
// restores a new pod from a snapshot when the pod matches a
// PodSnapshotPolicy and either carries the annotation
// podsnapshot.gke.io/ps-name or is the newest match. Two properties of that
// design shape this file:
//
//   - By default a new pod is restored from the newest snapshot whose pod
//     spec matches. Two tenants' sandboxes with the same image and resources
//     match, so a fresh sandbox could start from someone else's memory. The
//     pool therefore refuses a policy that does not select on, and group by,
//     the label snapshotGroup, which every sandbox carries with a value of its
//     own (a fork carries its source's).
//   - A pod that cannot be restored (another machine series, a different pod
//     spec, a snapshot that is not ready) starts fresh, silently. Before each
//     checkpoint the adapter writes a token to markerPath in the sandbox, and
//     after a restore it reads it back: a fork or a resume without it failed.

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
	// markerPath holds the token written before a checkpoint.
	markerPath = "/tmp/.differently-snapshot"

	annoToken    = "differently.dev/restore-token" // on a Sandbox: the token its pod must hold
	annoPaused   = "differently.dev/paused"        // on a Sandbox: the trigger of the pause it is in
	annoTemplate = "differently.dev/sandbox-spec"  // on a trigger: the source Sandbox's spec, for forks
	annoGroup    = "differently.dev/snapshot-group"
	labelPurpose = "differently.dev/snapshot-purpose" // on a trigger: snapshot or pause
	labelSource  = "differently.dev/snapshot-source"  // on a trigger: the Sandbox it checkpointed
)

// SnapshotFeatures are added when the pool sets config.podSnapshotPolicy.
var SnapshotFeatures = []string{
	adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork,
	adapter.FeaturePauseResume, adapter.FeaturePauseCheckpoint,
}

func (a *Adapter) snapPath(plural string) string {
	return "/apis/" + snapGroup + "/" + snapVersion + "/namespaces/" + url.PathEscape(a.opts.Namespace) + "/" + plural
}

func (a *Adapter) snapshots() bool { return a.opts.PodSnapshotPolicy != "" }

// policy is the part of a PodSnapshotPolicy the adapter checks.
type expression struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
}

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

// trigger is a PodSnapshotManualTrigger: the fields the adapter reads.
type trigger struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
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

// failed reports a trigger whose checkpoint ended without a snapshot.
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
	_, err := a.shell(ctx, ref, `printf '%s' "$1" > `+markerPath, nil, token)
	if err != nil {
		return fmt.Errorf("agent-sandbox: writing the snapshot marker %s in %s: %w", markerPath, ref, err)
	}
	return nil
}

func (a *Adapter) readMarker(ctx context.Context, ref string) (string, error) {
	out, err := a.shell(ctx, ref, `cat `+markerPath+` 2>/dev/null || true`, nil)
	return strings.TrimSpace(string(out)), err
}

// checkpoint writes the marker, triggers a snapshot of ref's pod and waits
// for the PodSnapshot to be ready. It returns the trigger's and the
// snapshot's names.
func (a *Adapter) checkpoint(ctx context.Context, ref, purpose string, annotations map[string]string) (trig, snap, token string, err error) {
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
			"labels":      map[string]string{labelPurpose: purpose, labelSource: ref},
			"annotations": annotations,
		},
		"spec": map[string]string{"targetPod": ref},
	}
	if err = a.k.do(ctx, http.MethodPost, a.snapPath("podsnapshotmanualtriggers"), "", body, nil); err != nil {
		return "", "", "", err
	}
	err = httpx.Poll(ctx, time.Second, a.opts.SnapshotTimeout, func() (bool, error) {
		var t trigger
		if err := a.k.do(ctx, http.MethodGet, a.snapPath("podsnapshotmanualtriggers")+"/"+url.PathEscape(trig), "", nil, &t); err != nil {
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
		_ = a.deleteCheckpoint(context.WithoutCancel(ctx), trig, snap)
		if api.IsCode(err, api.CodeProviderUnavailable) && strings.Contains(err.Error(), "timed out") {
			err = adapter.SandboxFailed(fmt.Errorf("agent-sandbox: the snapshot of %s was not ready within %s", ref, a.opts.SnapshotTimeout))
		}
		return "", "", "", err
	}
	return trig, snap, token, nil
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
		if err := a.k.do(ctx, http.MethodDelete, a.snapPath("podsnapshotmanualtriggers")+"/"+url.PathEscape(trig), "", nil, nil); err != nil && !api.IsCode(err, api.CodeNotFound) {
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
	if !a.snapshots() || a.opts.WarmPool != "" {
		return adapter.Unsupported(feature)
	}
	return nil
}

// sandboxSpec is what a fork copies from its source: the pod spec, the
// volume claim templates and the Service flag. GKE restores a pod only when
// its spec matches the snapshot's, so a fork does not take the request's
// image, resources or env.
type sandboxSpec struct {
	PodSpec              json.RawMessage `json:"podSpec"`
	VolumeClaimTemplates json.RawMessage `json:"volumeClaimTemplates,omitempty"`
	Service              json.RawMessage `json:"service,omitempty"`
}

func (a *Adapter) readSpec(ctx context.Context, ref string) (sandboxSpec, string, error) {
	var s struct {
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
		return sandboxSpec{}, "", err
	}
	group := s.Spec.PodTemplate.Metadata.Labels[snapshotGroup]
	if group == "" {
		return sandboxSpec{}, "", api.Errorf(api.CodeInvalidState, "agent-sandbox: %s was created without snapshots enabled (no %s label)", ref, snapshotGroup)
	}
	return sandboxSpec{PodSpec: s.Spec.PodTemplate.Spec, VolumeClaimTemplates: s.Spec.VolumeClaimTemplates, Service: s.Spec.Service}, group, nil
}

// Snapshot checkpoints the sandbox's whole pod: memory, processes and its
// root filesystem. Volumes are not captured. The sandbox keeps running.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if err := a.needSnapshots(adapter.FeatureSnapshotMem); err != nil {
		return adapter.Snapshot{}, err
	}
	spec, group, err := a.readSpec(ctx, ref)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	trig, snap, _, err := a.checkpoint(ctx, ref, "snapshot", map[string]string{annoTemplate: string(raw), annoGroup: group})
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

// forkManifest is a Sandbox restored from a snapshot: the source's spec, in
// the source's snapshot group, naming the snapshot to restore.
func (a *Adapter) forkManifest(ctx context.Context, req adapter.CreateRequest) (map[string]any, string, error) {
	snap, trig, err := parseSnapshotRef(req.FromSnapshot)
	if err != nil {
		return nil, "", err
	}
	var t trigger
	if err := a.k.do(ctx, http.MethodGet, a.snapPath("podsnapshotmanualtriggers")+"/"+url.PathEscape(trig), "", nil, &t); err != nil {
		return nil, "", err
	}
	if t.created() != snap || t.Metadata.Annotations[annoTemplate] == "" {
		return nil, "", api.Errorf(api.CodeInvalidRequest, "agent-sandbox: snapshot %s cannot be forked", req.FromSnapshot)
	}
	if ok, err := a.snapshotReady(ctx, snap); err != nil || !ok {
		if err == nil {
			err = api.Errorf(api.CodeInvalidState, "agent-sandbox: snapshot %s is not ready", snap)
		}
		return nil, "", err
	}
	var spec sandboxSpec
	if err := json.Unmarshal([]byte(t.Metadata.Annotations[annoTemplate]), &spec); err != nil {
		return nil, "", fmt.Errorf("agent-sandbox: snapshot %s: %w", snap, err)
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
		"metadata": map[string]any{"name": name(req.ID), "labels": labels, "annotations": map[string]string{annoToken: token}},
		"spec":     sbSpec,
	}, token, nil
}

// verifyRestore reads the marker back from a restored pod.
func (a *Adapter) verifyRestore(ctx context.Context, ref, token string) error {
	got, err := a.readMarker(ctx, ref)
	if err != nil {
		return err
	}
	if got != token {
		return adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s started without its snapshot (GKE restores only onto the same machine series, with the same pod spec, from a ready snapshot); its memory and files were not restored", ref))
	}
	return nil
}

// Pause checkpoints the sandbox and suspends it: the pod is deleted, so the
// sandbox holds no node resources while paused. Resume restores that
// checkpoint. Only the newest pause checkpoint is kept.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if err := a.needSnapshots(adapter.FeaturePauseResume); err != nil {
		return err
	}
	s, err := a.get(ctx, a.sandboxes(), ref)
	if err != nil {
		return err
	}
	if s.Metadata.Annotations[annoPaused] != "" && s.Spec.OperatingMode == "Suspended" {
		return nil // already paused
	}
	if s.Spec.OperatingMode == "Suspended" {
		return api.Errorf(api.CodeInvalidState, "agent-sandbox: %s is stopped", ref)
	}
	trig, snap, token, err := a.checkpoint(ctx, ref, "pause", map[string]string{})
	if err != nil {
		return err
	}
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{annoPaused: trig, annoToken: token}},
		"spec": map[string]any{
			"operatingMode": "Suspended",
			// The pod the controller creates on resume restores this snapshot.
			"podTemplate": map[string]any{"metadata": map[string]any{"annotations": map[string]string{psName: snap}}},
		},
	}
	if err := a.k.do(ctx, http.MethodPatch, a.sandboxes()+"/"+url.PathEscape(ref), "application/merge-patch+json", patch, nil); err != nil {
		_ = a.deleteCheckpoint(context.WithoutCancel(ctx), trig, snap)
		return err
	}
	if err := httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		s, err := a.get(ctx, a.sandboxes(), ref)
		return err == nil && s.is("Suspended"), err
	}); err != nil {
		return err
	}
	a.dropCheckpoints(ctx, ref, "pause", trig)
	return nil
}

// Resume restarts the pod, which GKE restores from the pause checkpoint, and
// checks that it did. If it did not, the sandbox is suspended again, so it
// never runs on without its state, and the error says why; a later resume
// tries again.
func (a *Adapter) Resume(ctx context.Context, ref string) error {
	if err := a.needSnapshots(adapter.FeaturePauseResume); err != nil {
		return err
	}
	s, err := a.get(ctx, a.sandboxes(), ref)
	if err != nil {
		return err
	}
	if s.Metadata.Annotations[annoPaused] == "" {
		if s.Spec.OperatingMode == "Suspended" {
			return api.Errorf(api.CodeInvalidState, "agent-sandbox: %s is stopped, not paused", ref)
		}
		return nil // running
	}
	token := s.Metadata.Annotations[annoToken]
	if s.Spec.OperatingMode == "Suspended" {
		if err := a.patch(ctx, a.sandboxes(), ref, map[string]any{"operatingMode": "Running"}); err != nil {
			return err
		}
	}
	if _, err := a.waitReady(ctx, ref); err != nil {
		return err
	}
	if err := a.verifyRestore(ctx, ref, token); err != nil {
		_ = a.patch(context.WithoutCancel(ctx), a.sandboxes(), ref, map[string]any{"operatingMode": "Suspended"})
		return err
	}
	// The checkpoint stays until the next pause or the delete: GKE may still be
	// streaming memory into the pod from it.
	return a.k.do(ctx, http.MethodPatch, a.sandboxes()+"/"+url.PathEscape(ref), "application/merge-patch+json",
		map[string]any{"metadata": map[string]any{"annotations": map[string]any{annoPaused: nil}}}, nil)
}

// dropCheckpoints deletes ref's checkpoints of one purpose except keep.
// Failures are left for the next call: a leftover costs storage only.
func (a *Adapter) dropCheckpoints(ctx context.Context, ref, purpose, keep string) {
	var list struct {
		Items []trigger `json:"items"`
	}
	q := url.Values{"labelSelector": {labelPurpose + "=" + purpose + "," + labelSource + "=" + ref}}
	if err := a.k.do(ctx, http.MethodGet, a.snapPath("podsnapshotmanualtriggers")+"?"+q.Encode(), "", nil, &list); err != nil {
		return
	}
	for _, t := range list.Items {
		if t.Metadata.Name != keep {
			_ = a.deleteCheckpoint(ctx, t.Metadata.Name, t.created())
		}
	}
}

var (
	_ adapter.Pauser      = (*Adapter)(nil)
	_ adapter.Snapshotter = (*Adapter)(nil)
)
