package sim

import (
	"time"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// hook is a scripted action: at a time, or the first tick a condition holds.
type hook struct {
	name string
	at   time.Time
	cond func() bool
	fn   func()
	done bool
}

// At runs fn once at elapsed time d after Epoch (d is relative to the
// current time if it already passed... use After for relative times).
func (w *World) At(d time.Duration, name string, fn func()) {
	w.hooks = append(w.hooks, &hook{name: name, at: Epoch.Add(d), fn: fn})
}

// After runs fn once, d after now.
func (w *World) After(d time.Duration, name string, fn func()) {
	w.hooks = append(w.hooks, &hook{name: name, at: w.Now().Add(d), fn: fn})
}

// When runs fn once, on the first tick cond holds.
func (w *World) When(name string, cond func() bool, fn func()) {
	w.hooks = append(w.hooks, &hook{name: name, cond: cond, fn: fn})
}

// OnStage runs fn once, on the first tick some remediation is in stage.
func (w *World) OnStage(stage v1alpha1.Stage, name string, fn func()) {
	w.When(name, func() bool { return w.ActiveStage() == stage }, fn)
}

// ActiveStage is the stage of the oldest non-terminal remediation ("" if none).
func (w *World) ActiveStage() v1alpha1.Stage {
	for _, r := range w.remediations() {
		if !r.Status.Stage.Terminal() {
			return r.Status.Stage
		}
	}
	return ""
}

// ActiveRemediation returns the name of the oldest non-terminal remediation.
func (w *World) ActiveRemediation() string {
	for _, r := range w.remediations() {
		if !r.Status.Stage.Terminal() {
			return r.Name
		}
	}
	return ""
}

func (w *World) runHooks() {
	rems := w.remediations()
	w.remsMemo = &rems
	defer func() { w.remsMemo = nil }()
	for _, h := range w.hooks {
		if h.done {
			continue
		}
		switch {
		case h.cond != nil:
			if !h.cond() {
				continue
			}
		case w.Now().Before(h.at):
			continue
		}
		h.done = true
		w.remsMemo = nil // the action may change the world
		if h.name != "" {
			w.trace.add(Event{Kind: "inject", Msg: h.name})
		}
		h.fn()
	}
}
