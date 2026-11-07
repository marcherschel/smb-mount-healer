package fake

import (
	"context"
	"slices"
	"sync"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// Prober implements hostfs.Prober. Like the real runner it first checks the
// snapshot (not_mountpoint, wrong_fstype); then, per path: a hung path
// returns ProbeHung, else the next scripted class is consumed, else the
// sticky result set by SetResult, else ProbeOK.
type Prober struct {
	ExpectedFSTypes []string

	mu     sync.Mutex
	script map[string][]hostfs.ProbeClass
	sticky map[string]hostfs.ProbeResult
	hung   map[string]bool
	calls  map[string]int
}

// Script queues one-shot classes for path, consumed in order.
func (p *Prober) Script(path string, classes ...hostfs.ProbeClass) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.script == nil {
		p.script = map[string][]hostfs.ProbeClass{}
	}
	p.script[path] = append(p.script[path], classes...)
}

// SetResult makes every later probe of path (after the structural checks and
// the script) return class with detail. ProbeOK clears it.
func (p *Prober) SetResult(path string, class hostfs.ProbeClass, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sticky == nil {
		p.sticky = map[string]hostfs.ProbeResult{}
	}
	if class == hostfs.ProbeOK {
		delete(p.sticky, path)
		return
	}
	p.sticky[path] = hostfs.ProbeResult{Class: class, Detail: detail}
}

// SetHung marks path as having a hung child (or clears it).
func (p *Prober) SetHung(path string, hung bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hung == nil {
		p.hung = map[string]bool{}
	}
	if hung {
		p.hung[path] = true
	} else {
		delete(p.hung, path)
	}
}

// Calls returns how many times path was probed.
func (p *Prober) Calls(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[path]
}

// Probe implements hostfs.Prober.
func (p *Prober) Probe(_ context.Context, path string, snap hostfs.MountSnapshot) hostfs.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[path]++
	res := hostfs.ProbeResult{Path: path, Class: hostfs.ProbeOK}
	e, ok := snap.Lookup(path)
	if !ok {
		res.Class = hostfs.ProbeNotMountpoint
		return res
	}
	res.FSType = e.FSType
	switch {
	case !slices.Contains(p.ExpectedFSTypes, e.FSType):
		res.Class = hostfs.ProbeWrongFSType
	case p.hung[path]:
		res.Class, res.Detail = hostfs.ProbeHung, "fake: hung"
	case len(p.script[path]) > 0:
		res.Class = p.script[path][0]
		p.script[path] = p.script[path][1:]
	default:
		if s, ok := p.sticky[path]; ok {
			res.Class, res.Detail = s.Class, s.Detail
		}
	}
	return res
}

// Hung implements hostfs.Prober.
func (p *Prober) Hung() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hung)
}
