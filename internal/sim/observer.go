package sim

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// observer turns CR state changes into trace lines and records outcomes.
type observer struct {
	w     *World
	smbm  map[string]string
	rems  map[string]*remSeen
	tasks map[string]v1alpha1.HostTaskPhase
	smbq  map[string]string
	// outcomes in remediation creation order.
	order    []string
	outcomes map[string]v1alpha1.Outcome
	types    map[string]v1alpha1.RemediationType
}

type remSeen struct {
	trans map[string]bool
	scope string
	verif string
}

func newObserver(w *World) *observer {
	return &observer{w: w, smbm: map[string]string{}, rems: map[string]*remSeen{}, tasks: map[string]v1alpha1.HostTaskPhase{},
		smbq: map[string]string{}, outcomes: map[string]v1alpha1.Outcome{}, types: map[string]v1alpha1.RemediationType{}}
}

// resetQuiet re-reads the state without tracing (end of warm-up), so the
// trace starts from the baseline.
func (o *observer) resetQuiet() {
	en := o.w.trace.enabled
	o.w.trace.enabled = false
	o.observe()
	o.w.trace.enabled = en
}

func (o *observer) observe() {
	o.observeRemediations()
	o.observeTasks()
	o.observeSmbMounts()
	o.observeQuarantines()
}

func (o *observer) observeSmbMounts() {
	w := o.w
	seen := map[string]bool{}
	mounts := w.smbMounts()
	for i := range mounts {
		sm := &mounts[i]
		seen[sm.Name] = true
		s := o.smbmSummary(sm)
		if prev, ok := o.smbm[sm.Name]; ok && prev == s {
			continue
		}
		o.smbm[sm.Name] = s
		w.trace.add(Event{Kind: "smbm", Obj: sm.Name, Msg: s})
	}
	for _, name := range sortedKeys(o.smbm) {
		if !seen[name] {
			delete(o.smbm, name)
			w.trace.add(Event{Kind: "smbm", Obj: name, Msg: "deleted"})
		}
	}
}

func (o *observer) smbmSummary(sm *v1alpha1.SmbMount) string {
	st := &sm.Status
	h := string(st.Health)
	if st.Reason != "" {
		h += "(" + string(st.Reason) + ")"
	}
	parts := []string{h}
	gm := "gm=?"
	if g := st.Globalmount; g != nil {
		switch {
		case g.Mounted:
			gm = "gm=" + g.FSType
		case !g.DirExists:
			gm = "gm=absent"
		case g.Empty:
			gm = "gm=empty-dir"
		default:
			gm = "gm=dir-with-data"
		}
	}
	parts = append(parts, gm)
	var pods []string
	for _, pm := range st.PodMounts {
		p := pm.Pod + ":" + string(pm.Health)
		if pm.Reason != "" {
			p += "(" + string(pm.Reason) + ")"
		}
		pods = append(pods, p)
	}
	if len(pods) > 0 {
		parts = append(parts, "pods=["+strings.Join(pods, " ")+"]")
	}
	if n := len(st.OrphanPodDirs); n > 0 {
		full := 0
		for _, od := range st.OrphanPodDirs {
			if od.Mounted || !od.Empty {
				full++
			}
		}
		parts = append(parts, fmt.Sprintf("orphans=%d(dirty=%d)", n, full))
	}
	if st.PendingQuarantines > 0 {
		parts = append(parts, fmt.Sprintf("quarantines=%d", st.PendingQuarantines))
	}
	if r := st.Remediation; r != nil && r.Active {
		parts = append(parts, "stage="+string(r.Stage))
	}
	if l := st.LastRemediation; l != nil && l.Outcome != "" {
		parts = append(parts, "last="+string(l.Outcome))
	}
	return strings.Join(parts, " ")
}

func (o *observer) observeRemediations() {
	w := o.w
	rems := w.remediations()
	// Creation order: creationTimestamp, then name.
	sort.SliceStable(rems, func(i, j int) bool {
		a, b := rems[i].CreationTimestamp, rems[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return rems[i].Name < rems[j].Name
	})
	seen := map[string]bool{}
	for i := range rems {
		seen[rems[i].Name] = true
		o.observeRemediation(&rems[i])
	}
	for _, name := range sortedKeys(o.rems) {
		if !seen[name] {
			delete(o.rems, name)
			w.trace.add(Event{Kind: "smbr", Obj: name, Msg: "deleted"})
		}
	}
}

// observeRemediation traces what changed on one remediation. It also runs
// on every status write of the control plane (so a remediation deleted
// right after reaching done still shows its last stages and outcome).
func (o *observer) observeRemediation(r *v1alpha1.SmbMountRemediation) {
	w := o.w
	rs := o.rems[r.Name]
	if rs == nil {
		rs = &remSeen{trans: map[string]bool{}}
		o.rems[r.Name] = rs
		o.order = append(o.order, r.Name)
		o.types[r.Name] = r.Spec.Type
		w.trace.add(Event{Kind: "smbr", Obj: r.Name, Msg: fmt.Sprintf("created type=%s smbMounts=%v reasons=%v dryRun=%t",
			r.Spec.Type, r.Spec.Trigger.SmbMounts, r.Spec.Trigger.Reasons, r.Spec.DryRun)})
	}
	for _, t := range r.Status.Transitions {
		k := t.Time.UTC().String() + "/" + string(t.From) + "/" + string(t.To) + "/" + t.Reason
		if rs.trans[k] {
			continue
		}
		rs.trans[k] = true
		w.trace.add(Event{Kind: "stage", Obj: r.Name, From: string(t.From), To: string(t.To), Reason: t.Reason})
	}
	if s := scopeSummary(r); s != rs.scope {
		rs.scope = s
		w.trace.add(Event{Kind: "scope", Obj: r.Name, Msg: s})
	}
	if v := verifSummary(r); v != rs.verif {
		rs.verif = v
		w.trace.add(Event{Kind: "verify", Obj: r.Name, Msg: v})
	}
	if r.Status.Outcome != "" && o.outcomes[r.Name] == "" {
		o.outcomes[r.Name] = r.Status.Outcome
		w.trace.add(Event{Kind: "outcome", Obj: r.Name, To: string(r.Status.Outcome), Msg: w.trace.abbrevAll(r.Status.Message)})
	}
}

func scopeSummary(r *v1alpha1.SmbMountRemediation) string {
	sc := r.Status.Scope
	if sc == nil {
		return ""
	}
	var wls []string
	for _, wl := range sc.Workloads {
		wls = append(wls, fmt.Sprintf("%s(orig=%d,%s)", wl.String(), wl.OriginalReplicas, wl.State))
	}
	var drain []string
	for _, p := range sc.DrainPods {
		drain = append(drain, p.Name)
	}
	s := fmt.Sprintf("workloads=[%s] pvs=%v drain=[%s]", strings.Join(wls, " "), sc.PVs, strings.Join(drain, " "))
	if len(r.Status.DeadGlobalmounts) > 0 {
		var d []string
		for _, g := range r.Status.DeadGlobalmounts {
			d = append(d, g.PVName)
		}
		s += " deadGlobalmounts=" + strings.Join(d, ",")
	}
	return s
}

func verifSummary(r *v1alpha1.SmbMountRemediation) string {
	var out []string
	for _, v := range r.Status.Verification {
		out = append(out, v.Workload.String()+"="+string(v.Result))
	}
	return strings.Join(out, " ")
}

func (o *observer) observeTasks() {
	w := o.w
	var list v1alpha1.SmbHostTaskList
	if err := w.store.List(w.ctx, &list); err != nil {
		w.violation("list tasks: %v", err)
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	seen := map[string]bool{}
	for i := range list.Items {
		t := &list.Items[i]
		seen[t.Name] = true
		ph := t.Status.Phase
		if o.tasks[t.Name] == ph {
			continue
		}
		o.tasks[t.Name] = ph
		if ph != v1alpha1.HostTaskSucceeded && ph != v1alpha1.HostTaskFailed {
			continue
		}
		var res []string
		for _, r := range t.Status.Results {
			s := r.ID + "=" + string(r.Result)
			if r.Destination != "" {
				s += fmt.Sprintf("->%s(%d entries)", w.trace.abbrev(r.Destination), r.Entries)
			}
			if r.Error != "" {
				s += "(" + w.trace.abbrevAll(r.Error) + ")"
			}
			res = append(res, s)
		}
		dry := ""
		if t.Spec.DryRun {
			dry = " dryRun"
		}
		w.trace.add(Event{Kind: "smbt", Obj: t.Name, To: string(ph), Msg: "[" + strings.Join(res, " ") + "]" + dry})
	}
	for _, name := range sortedKeys(o.tasks) {
		if !seen[name] {
			delete(o.tasks, name)
		}
	}
}

func (o *observer) observeQuarantines() {
	w := o.w
	var list v1alpha1.SmbQuarantineList
	if err := w.store.List(w.ctx, &list); err != nil {
		w.violation("list quarantines: %v", err)
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	seen := map[string]bool{}
	for i := range list.Items {
		q := &list.Items[i]
		seen[q.Name] = true
		s := fmt.Sprintf("pv=%s files=%d entries=%d bytes=%d source=%s remediationRef=%q",
			q.Spec.PVName, q.Status.Files, q.Status.Entries, q.Status.Bytes, w.trace.abbrev(q.Spec.SourcePath), q.Spec.RemediationRef)
		prev, ok := o.smbq[q.Name]
		cur := string(q.UID) + "|" + s
		if ok && prev == cur {
			continue
		}
		o.smbq[q.Name] = cur
		verb := "updated "
		switch {
		case !ok:
			verb = "created "
		case !strings.HasPrefix(prev, string(q.UID)+"|"):
			verb = "recreated "
		}
		w.trace.add(Event{Kind: "smbq", Obj: q.Name, Msg: verb + s})
	}
	for _, name := range sortedKeys(o.smbq) {
		if !seen[name] {
			delete(o.smbq, name)
			w.trace.add(Event{Kind: "smbq", Obj: name, Msg: "deleted"})
		}
	}
}

// Outcomes returns the outcome of every remediation seen, in creation order
// ("" for unfinished ones).
func (w *World) Outcomes() []string {
	out := make([]string, 0, len(w.obs.order))
	for _, n := range w.obs.order {
		out = append(out, string(w.obs.types[n])+":"+string(w.obs.outcomes[n]))
	}
	return out
}

// --- invariants -------------------------------------------------------------

func (w *World) checkInvariants() {
	rems := w.remediations()
	active := map[string][]string{}
	owners := map[string]string{}
	anyActive := false
	for i := range rems {
		r := &rems[i]
		if r.Status.Stage.Terminal() {
			continue
		}
		anyActive = true
		active[r.Spec.NodeName] = append(active[r.Spec.NodeName], r.Name)
		if r.Status.Scope == nil || r.Status.Stage == v1alpha1.StagePending || r.Status.Stage == v1alpha1.StagePlanning || r.Status.Stage == "" {
			continue
		}
		for _, wl := range r.Status.Scope.Workloads {
			if other, ok := owners[wl.String()]; ok {
				w.violation("workload %s in the scope of two active remediations: %s and %s", wl.String(), other, r.Name)
			}
			owners[wl.String()] = r.Name
		}
	}
	for _, node := range sortedKeys(active) {
		if names := active[node]; len(names) > 1 {
			w.violation("node %s has %d active remediations: %v", node, len(names), names)
		}
	}
	if !anyActive || w.dryRun {
		for _, wl := range w.workloads {
			if wl.Kind == "" {
				continue
			}
			if r := wl.Replicas(); r != wl.Baseline {
				w.violation("workload %s at %d replicas, want %d (no active remediation or dry run)", wl.ref(), r, wl.Baseline)
			}
		}
	}
	if w.dryRun {
		for _, n := range w.Nodes {
			if calls := n.Host.Mutator.Calls(); len(calls) > 0 {
				w.violation("dry run: host mutated on %s: %+v", n.Name, calls)
			}
			if n.driverSeq > 1 {
				w.violation("dry run: CSI driver pod on %s was restarted", n.Name)
			}
		}
	}
	if int64(w.Elapsed().Seconds())%60 == 0 {
		w.checkCRPrivacy()
	}
}

// checkCRPrivacy dumps every healer CR and checks it for quarantined names.
func (w *World) checkCRPrivacy() {
	var a v1alpha1.SmbMountList
	var b v1alpha1.SmbMountRemediationList
	var c v1alpha1.SmbHostTaskList
	var d v1alpha1.SmbQuarantineList
	if err := w.store.List(w.ctx, &a); err != nil {
		w.violation("privacy: %v", err)
	}
	if err := w.store.List(w.ctx, &b); err != nil {
		w.violation("privacy: %v", err)
	}
	if err := w.store.List(w.ctx, &c); err != nil {
		w.violation("privacy: %v", err)
	}
	if err := w.store.List(w.ctx, &d); err != nil {
		w.violation("privacy: %v", err)
	}
	for _, obj := range []any{a, b, c, d} {
		raw, err := json.Marshal(obj)
		if err != nil {
			w.violation("privacy: marshal: %v", err)
			continue
		}
		w.checkSecret("custom resources", string(raw))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
