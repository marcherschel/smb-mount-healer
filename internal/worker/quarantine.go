package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// provenance is where a quarantine folder's data came from.
type provenance struct {
	sourcePath  string
	remediation string
}

// QuarantineScanner keeps one SmbQuarantine per non-empty quarantine folder
// of this node, reflecting what is on disk: it creates missing objects
// (including after a manual delete, and for folders left by the bash healer,
// adopted with an empty remediationRef), updates their aggregates, and
// deletes objects whose folder is gone or empty. It never deletes anything on
// disk and never puts a name from the quarantined data anywhere: only the
// folder path (the healer's own "<pv>-<UTC>" name), counts, sizes and times.
//
// It scans at startup, every QUARANTINE_SCAN_INTERVAL, right after a
// Quarantine host action and when an SmbQuarantine of this node is deleted.
type QuarantineScanner struct {
	w    *Worker
	kick signal

	scanMu  sync.Mutex // serialises Scan; guards pvs
	pvs     map[string]bool
	scanned atomic.Bool

	mu   sync.Mutex
	prov map[string]provenance // folder path -> provenance (from task results in this process)
}

func newScanner(w *Worker) *QuarantineScanner {
	return &QuarantineScanner{w: w, kick: newSignal(), pvs: map[string]bool{}, prov: map[string]provenance{}}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*QuarantineScanner) NeedLeaderElection() bool { return false }

// Trigger requests a scan as soon as possible. It never blocks.
func (s *QuarantineScanner) Trigger() { s.kick.notify() }

func (s *QuarantineScanner) scannedOnce() bool { return s.scanned.Load() }

// recordProvenance remembers where the data in folder came from, so the
// SmbQuarantine created for it gets sourcePath and remediationRef even
// before the SmbHostTask result is visible in the cache.
func (s *QuarantineScanner) recordProvenance(folder string, p provenance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prov[folder] = p
}

// Start implements manager.Runnable.
func (s *QuarantineScanner) Start(ctx context.Context) error {
	log := s.w.log.WithName("quarantine")
	ctx = logr.NewContext(ctx, log)
	if !s.w.cache.WaitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("quarantine scanner: cache did not sync")
	}
	inf, err := s.w.cache.GetInformer(ctx, &v1alpha1.SmbQuarantine{})
	if err != nil {
		return err
	}
	if _, err := inf.AddEventHandler(deleteHandler(s.Trigger)); err != nil {
		return err
	}
	for {
		if err := s.Scan(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "quarantine scan failed")
		}
		timer := s.w.clock.NewTimer(s.w.cfg.QuarantineScanInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-s.kick:
		case <-timer.C():
		}
		timer.Stop()
	}
}

// folderState is one quarantine folder found by a scan.
type folderState struct {
	folder hostfs.QuarantineFolder
	pv     string
	ts     time.Time
	stats  hostfs.QuarantineStats
	failed bool // the scan of this folder failed; keep its object as is
}

// Scan reconciles this node's SmbQuarantine objects with QUARANTINE_DIR.
func (s *QuarantineScanner) Scan(ctx context.Context) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	err := s.scan(ctx)
	result := metrics.ResultSuccess
	if err != nil {
		result = metrics.ResultError
	}
	s.w.m.QuarantineScans.WithLabelValues(result).Inc()
	s.scanned.Store(true)
	return err
}

func (s *QuarantineScanner) scan(ctx context.Context) error {
	log := s.w.logger(ctx)
	w := s.w
	folders, err := w.host.Quarantine.ListFolders(ctx)
	if err != nil {
		return fmt.Errorf("list quarantine folders: %w", err)
	}
	var errs []error
	var found []folderState
	ignored := 0
	for _, f := range folders {
		pv, ts, ok := kubelet.ParseQuarantineFolderName(f.Name)
		if !ok {
			ignored++ // not a healer folder: never touched, never named
			continue
		}
		st := folderState{folder: f, pv: pv, ts: ts}
		st.stats, err = w.host.Quarantine.Scan(ctx, f.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // moved away between list and scan
		case err != nil:
			st.failed = true
			errs = append(errs, fmt.Errorf("scan quarantine folder %s: %w", f.Path, err))
		case st.stats.Entries == 0:
			continue // emptied: resolved
		}
		found = append(found, st)
	}
	if ignored > 0 {
		log.V(1).Info("ignoring folders in the quarantine dir that are not healer folders", "count", ignored)
	}

	owner, err := w.nodeOwner(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	var list v1alpha1.SmbQuarantineList
	if err := w.c.List(ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: w.node}); err != nil {
		return errors.Join(append(errs, fmt.Errorf("list SmbQuarantines: %w", err))...)
	}
	live := map[string]*v1alpha1.SmbQuarantine{}
	for i := range list.Items {
		if q := &list.Items[i]; q.Spec.NodeName == w.node {
			live[q.Name] = q
		}
	}
	prov, err := s.provenance(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	shares, err := s.shares(ctx)
	if err != nil {
		errs = append(errs, err)
	}

	now := metav1.NewTime(w.clock.Now())
	keep := map[string]bool{}
	type agg struct {
		count  int
		bytes  int64
		oldest time.Time
	}
	perPV := map[string]*agg{}
	for _, f := range found {
		name := v1alpha1.SmbQuarantineName(w.node, f.folder.Name)
		keep[name] = true
		a := perPV[f.pv]
		if a == nil {
			a = &agg{oldest: f.ts}
			perPV[f.pv] = a
		}
		a.count++
		if f.ts.Before(a.oldest) {
			a.oldest = f.ts
		}
		q := live[name]
		if f.failed {
			if q != nil {
				a.bytes += q.Status.Bytes
			}
			continue
		}
		a.bytes += f.stats.Bytes
		switch {
		case q != nil && q.Spec.Path != f.folder.Path:
			// Two folders whose names differ only in case map to one object.
			errs = append(errs, fmt.Errorf("SmbQuarantine %s already tracks %s, not %s", name, q.Spec.Path, f.folder.Path))
			continue
		case q != nil && !q.DeletionTimestamp.IsZero():
			continue // recreated by the next scan
		case q == nil:
			q, err = s.create(ctx, name, f, owner, prov[f.folder.Path], shares[f.pv])
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := s.updateStatus(ctx, q, f.stats, now); err != nil {
			errs = append(errs, err)
		}
	}
	for name, q := range live {
		if keep[name] || !q.DeletionTimestamp.IsZero() {
			continue
		}
		log.Info("deleting SmbQuarantine: its folder is gone or empty", "smbQuarantine", name, "path", q.Spec.Path)
		if err := w.c.Delete(ctx, q, client.Preconditions{UID: &q.UID}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			errs = append(errs, fmt.Errorf("delete SmbQuarantine %s: %w", name, err))
		}
	}

	for pv := range s.pvs {
		if perPV[pv] == nil {
			w.m.Quarantines.DeleteLabelValues(pv)
			w.m.QuarantineBytes.DeleteLabelValues(pv)
			w.m.QuarantineOldestTimestamp.DeleteLabelValues(pv)
			delete(s.pvs, pv)
		}
	}
	for pv, a := range perPV {
		w.m.Quarantines.WithLabelValues(pv).Set(float64(a.count))
		w.m.QuarantineBytes.WithLabelValues(pv).Set(float64(a.bytes))
		w.m.QuarantineOldestTimestamp.WithLabelValues(pv).Set(float64(a.oldest.Unix()))
		s.pvs[pv] = true
	}
	w.Publisher.kick.notify()
	return errors.Join(errs...)
}

func (s *QuarantineScanner) create(ctx context.Context, name string, f folderState, owner metav1.OwnerReference,
	p provenance, share string,
) (*v1alpha1.SmbQuarantine, error) {
	q := &v1alpha1.SmbQuarantine{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Labels:          s.w.objectLabels(f.pv),
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: v1alpha1.SmbQuarantineSpec{
			NodeName:        s.w.node,
			Path:            f.folder.Path,
			PVName:          f.pv,
			Share:           share,
			SourcePath:      p.sourcePath,
			QuarantinedTime: metav1.NewTime(f.ts),
			RemediationRef:  p.remediation,
		},
	}
	if err := s.w.c.Create(ctx, q, client.FieldOwner(v1alpha1.FieldManagerWorker)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Created by an earlier scan; the cache is behind. Update it on
			// the next scan.
			return nil, nil //nolint:nilnil // handled by the caller
		}
		return nil, fmt.Errorf("create SmbQuarantine %s: %w", name, err)
	}
	adopted := p.remediation == ""
	s.w.logger(ctx).Info("created SmbQuarantine; quarantined shadow data is pending recovery",
		"smbQuarantine", name, "path", f.folder.Path, "pv", f.pv, "remediation", p.remediation, "adopted", adopted,
		"entries", f.stats.Entries, "files", f.stats.Files, "bytes", f.stats.Bytes)
	s.w.event(q, corev1.EventTypeWarning, EventShadowDataQuarantine,
		fmt.Sprintf("quarantine folder %s holds %d file(s), %d byte(s) of shadow data of PV %s; move the data off the node to resolve",
			f.folder.Path, f.stats.Files, f.stats.Bytes, f.pv))
	return q, nil
}

func (s *QuarantineScanner) updateStatus(ctx context.Context, q *v1alpha1.SmbQuarantine, st hostfs.QuarantineStats, now metav1.Time) error {
	if q == nil {
		return nil
	}
	q = q.DeepCopy()
	status := v1alpha1.SmbQuarantineStatus{
		Entries:       st.Entries,
		Files:         st.Files,
		Bytes:         st.Bytes,
		SizeTruncated: st.SizeTruncated,
		LastScanTime:  &now,
	}
	if !st.OldestModTime.IsZero() {
		t := metav1.NewTime(st.OldestModTime)
		status.OldestModTime = &t
	}
	if !st.NewestModTime.IsZero() {
		t := metav1.NewTime(st.NewestModTime)
		status.NewestModTime = &t
	}
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionPending,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonDataPresent,
		Message:            "quarantined data is still present; move it off the node to resolve this object",
		LastTransitionTime: now,
		ObservedGeneration: q.Generation,
	}
	for _, c := range q.Status.Conditions {
		if c.Type == cond.Type && c.Status == cond.Status && !c.LastTransitionTime.IsZero() {
			cond.LastTransitionTime = c.LastTransitionTime
		}
	}
	status.Conditions = []metav1.Condition{cond}
	q.Status = status
	if err := s.w.c.Status().Update(ctx, q); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("update status of SmbQuarantine %s: %w", q.Name, err)
	}
	return nil
}

// provenance merges the in-process records with the destinations recorded
// in this node's SmbHostTask results (survives a worker restart).
func (s *QuarantineScanner) provenance(ctx context.Context) (map[string]provenance, error) {
	s.mu.Lock()
	out := maps.Clone(s.prov)
	s.mu.Unlock()
	var tasks v1alpha1.SmbHostTaskList
	if err := s.w.c.List(ctx, &tasks, client.MatchingLabels{v1alpha1.LabelNode: s.w.node}); err != nil {
		return out, fmt.Errorf("list SmbHostTasks: %w", err)
	}
	for i := range tasks.Items {
		t := &tasks.Items[i]
		paths := map[string]string{}
		for _, a := range t.Spec.Actions {
			paths[a.ID] = a.Path
		}
		for _, r := range t.Status.Results {
			if r.Destination == "" {
				continue
			}
			if _, ok := out[r.Destination]; !ok {
				out[r.Destination] = provenance{sourcePath: paths[r.ID], remediation: taskRemediation(t)}
			}
		}
	}
	return out, nil
}

// shares maps PV names to their SMB source.
func (s *QuarantineScanner) shares(ctx context.Context) (map[string]string, error) {
	var pvs corev1.PersistentVolumeList
	if err := s.w.c.List(ctx, &pvs); err != nil {
		return nil, fmt.Errorf("list PVs: %w", err)
	}
	out := map[string]string{}
	for i := range pvs.Items {
		if csi := pvs.Items[i].Spec.CSI; csi != nil && csi.Driver == s.w.layout.DriverName {
			out[pvs.Items[i].Name] = csi.VolumeAttributes["source"]
		}
	}
	return out, nil
}

// taskRemediation is the SmbMountRemediation that created t.
func taskRemediation(t *v1alpha1.SmbHostTask) string {
	if r := t.Labels[v1alpha1.LabelRemediation]; r != "" {
		return r
	}
	for _, o := range t.OwnerReferences {
		if o.Kind == "SmbMountRemediation" {
			return o.Name
		}
	}
	return ""
}
