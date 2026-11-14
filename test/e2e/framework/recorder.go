//go:build e2e

package framework

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// Observation is one observed state of an SmbMount (one watch event).
type Observation struct {
	Time        time.Time
	Deleted     bool
	Health      v1alpha1.Health
	Reason      v1alpha1.FailureReason
	Stage       v1alpha1.Stage // status.remediation.stage while active (the STAGE column)
	Remediation string         // status.remediation.name while active
	LastName    string         // status.lastRemediation.name
	LastOutcome v1alpha1.Outcome
	PodMounts   []v1alpha1.PodMountStatus
}

// Recorder watches every SmbMount for the lifetime of a test and keeps the
// sequence of states per object, so a test can assert that an SmbMount went
// through Broken (or Suspect) even when that state lasted only seconds.
type Recorder struct {
	f    *F
	mu   sync.Mutex
	obs  map[string][]Observation
	err  error
	stop chan struct{}
	done chan struct{}
}

// RecordSmbMounts starts a Recorder; it is stopped by a test cleanup.
func (f *F) RecordSmbMounts() *Recorder {
	f.T.Helper()
	wc, err := client.NewWithWatch(f.Config, client.Options{Scheme: f.Client.Scheme()})
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	r := &Recorder{f: f, obs: map[string][]Observation{}, stop: make(chan struct{}), done: make(chan struct{})}
	started := make(chan struct{})
	go r.run(wc, started)
	<-started
	f.T.Cleanup(r.Stop)
	return r
}

// Stop ends the watch (idempotent).
func (r *Recorder) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

func (r *Recorder) run(wc client.WithWatch, started chan struct{}) {
	defer close(r.done)
	once := sync.Once{}
	signal := func() { once.Do(func() { close(started) }) }
	defer signal()
	for {
		select {
		case <-r.stop:
			return
		case <-r.f.Ctx.Done():
			return
		default:
		}
		var l v1alpha1.SmbMountList
		if err := wc.List(r.f.Ctx, &l); err != nil {
			r.setErr(err)
			if !r.wait(time.Second) {
				return
			}
			continue
		}
		for i := range l.Items {
			r.add(&l.Items[i], false)
		}
		w, err := wc.Watch(r.f.Ctx, &v1alpha1.SmbMountList{}, &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: l.ResourceVersion}})
		if err != nil {
			r.setErr(err)
			if !r.wait(time.Second) {
				return
			}
			continue
		}
		signal()
		if !r.consume(w) {
			return
		}
	}
}

// consume reads events until the watch closes (true) or the recorder stops (false).
func (r *Recorder) consume(w watch.Interface) bool {
	defer w.Stop()
	for {
		select {
		case <-r.stop:
			return false
		case ev, ok := <-w.ResultChan():
			if !ok {
				return true
			}
			sm, isMount := ev.Object.(*v1alpha1.SmbMount)
			if !isMount {
				return true // watch error (e.g. resourceVersion too old): relist
			}
			r.add(sm, ev.Type == watch.Deleted)
		}
	}
}

// wait blocks for d or until the recorder stops; false if stopped.
func (r *Recorder) wait(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.stop:
		return false
	case <-t.C:
		return true
	}
}

func (r *Recorder) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *Recorder) add(sm *v1alpha1.SmbMount, deleted bool) {
	o := Observation{
		Time:      time.Now(),
		Deleted:   deleted,
		Health:    sm.Status.Health,
		Reason:    sm.Status.Reason,
		PodMounts: append([]v1alpha1.PodMountStatus(nil), sm.Status.PodMounts...),
	}
	if a := sm.Status.Remediation; a != nil && a.Active {
		o.Stage, o.Remediation = a.Stage, a.Name
	}
	if l := sm.Status.LastRemediation; l != nil {
		o.LastName, o.LastOutcome = l.Name, l.Outcome
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs[sm.Name] = append(r.obs[sm.Name], o)
}

// Of returns the observations of SmbMount name so far.
func (r *Recorder) Of(name string) []Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Observation(nil), r.obs[name]...)
}

// Healths is the de-duplicated sequence of top-level healths of name.
func (r *Recorder) Healths(name string) []v1alpha1.Health {
	var out []v1alpha1.Health
	for _, o := range r.Of(name) {
		if o.Deleted {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != o.Health {
			out = append(out, o.Health)
		}
	}
	return out
}

// Stages is the de-duplicated sequence of STAGE values of name while a
// remediation (any, or only remediation if non-empty) was active.
func (r *Recorder) Stages(name, remediation string) []v1alpha1.Stage {
	var out []v1alpha1.Stage
	for _, o := range r.Of(name) {
		if o.Stage == "" || (remediation != "" && o.Remediation != remediation) {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != o.Stage {
			out = append(out, o.Stage)
		}
	}
	return out
}

// FirstSeen returns when name was first observed with health h (top level,
// or on any pod mount if podMounts is set).
func (r *Recorder) FirstSeen(name string, h v1alpha1.Health, podMounts bool) (time.Time, bool) {
	for _, o := range r.Of(name) {
		if o.Health == h {
			return o.Time, true
		}
		if podMounts {
			for _, pm := range o.PodMounts {
				if pm.Health == h {
					return o.Time, true
				}
			}
		}
	}
	return time.Time{}, false
}

// SawHealth reports whether name was observed with health h (top level,
// or on any pod mount if podMounts is set).
func (r *Recorder) SawHealth(name string, h v1alpha1.Health, podMounts bool) bool {
	for _, o := range r.Of(name) {
		if o.Health == h {
			return true
		}
		if podMounts {
			for _, pm := range o.PodMounts {
				if pm.Health == h {
					return true
				}
			}
		}
	}
	return false
}

// Describe renders name's observations for failure messages.
func (r *Recorder) Describe(name string) string {
	var b strings.Builder
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	if err != nil {
		fmt.Fprintf(&b, "(last watch error: %v)\n", err)
	}
	for _, o := range r.Of(name) {
		var pms []string
		for _, pm := range o.PodMounts {
			pms = append(pms, fmt.Sprintf("%s:%s/%s/%s", pm.Pod, pm.FSType, pm.Health, pm.Reason))
		}
		fmt.Fprintf(&b, "%s deleted=%v health=%s reason=%s stage=%s(%s) last=%s/%s pods=[%s]\n",
			o.Time.Format("15:04:05.000"), o.Deleted, o.Health, o.Reason, o.Stage, o.Remediation,
			o.LastName, o.LastOutcome, strings.Join(pms, " "))
	}
	return b.String()
}
