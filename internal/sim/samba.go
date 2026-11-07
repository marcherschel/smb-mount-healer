package sim

import (
	"sort"
	"strings"
	"time"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// ServerState is the state of a fake samba server.
type ServerState string

const (
	// ServerUp serves every request.
	ServerUp ServerState = "up"
	// ServerDown refuses connections (pod deleted / scaled to 0): stat on a
	// soft cifs mount fails fast with an I/O error.
	ServerDown ServerState = "down"
	// ServerPaused is SIGSTOPped smbd: stat times out, and after
	// pausedHungAfter the probe child sticks in D-state (probe_hung).
	ServerPaused ServerState = "paused"
	// ServerSlow answers slower than STAT_TIMEOUT (stat_timeout, never hung).
	ServerSlow ServerState = "slow"
)

// pausedHungAfter is how long a paused server takes to wedge a probe child.
const pausedHungAfter = 10 * time.Second

type server struct {
	state ServerState
	since time.Time
	// gen is bumped when the server comes back with a new IP: every cifs
	// session established before is stale (stat_error) until remounted
	// through a CSI driver that knows the new address.
	gen   int
	files map[string]map[string]bool // share -> file names (heartbeats)
}

// Samba is the fake samba deployment: servers with shares, feeding probe
// results of the cifs mounts that point at them.
type Samba struct {
	w       *World
	servers map[string]*server
}

func newSamba(w *World) *Samba { return &Samba{w: w, servers: map[string]*server{}} }

func (s *Samba) server(name string) *server {
	sv := s.servers[name]
	if sv == nil {
		sv = &server{state: ServerUp, gen: 1, files: map[string]map[string]bool{}}
		s.servers[name] = sv
	}
	return sv
}

// Set changes a server's state.
func (s *Samba) Set(name string, st ServerState) {
	sv := s.server(name)
	if sv.state == st {
		return
	}
	s.w.trace.add(Event{Kind: "samba", Obj: name, From: string(sv.state), To: string(st)})
	sv.state, sv.since = st, s.w.Now()
}

// NewIP restarts a server under a new address: existing sessions go stale.
func (s *Samba) NewIP(name string) {
	sv := s.server(name)
	sv.gen++
	s.w.trace.add(Event{Kind: "samba", Obj: name, Msg: "restarted with a new IP; existing cifs sessions are stale"})
}

// Gen returns the current address generation of a server.
func (s *Samba) Gen(name string) int { return s.server(name).gen }

// gens snapshots every server's generation (what a freshly started CSI
// driver resolves).
func (s *Samba) gens() map[string]int {
	out := map[string]int{}
	for n, sv := range s.servers {
		out[n] = sv.gen
	}
	return out
}

// canMount reports whether a new cifs mount of server succeeds (a paused
// server makes mount(2) hang until kubelet's operation times out).
func (s *Samba) canMount(name string) bool {
	st := s.server(name).state
	return st == ServerUp || st == ServerSlow
}

// classify is the probe outcome of a cifs session on server established at gen.
func (s *Samba) classify(name string, gen int) (hostfs.ProbeClass, bool) {
	sv := s.server(name)
	if gen < sv.gen {
		return hostfs.ProbeStatError, false
	}
	switch sv.state {
	case ServerUp:
		return hostfs.ProbeOK, false
	case ServerDown:
		return hostfs.ProbeStatError, false
	case ServerPaused:
		if s.w.Now().Sub(sv.since) >= pausedHungAfter {
			return hostfs.ProbeHung, true
		}
		return hostfs.ProbeStatTimeout, false
	case ServerSlow:
		return hostfs.ProbeStatTimeout, false
	}
	return hostfs.ProbeStatError, false
}

// write records a heartbeat file in a share.
func (s *Samba) write(serverName, share, file string) {
	sv := s.server(serverName)
	if sv.files[share] == nil {
		sv.files[share] = map[string]bool{}
	}
	sv.files[share][file] = true
}

// Files returns the files in a share, sorted.
func (s *Samba) Files(serverName, share string) []string {
	var out []string
	for f := range s.server(serverName).files[share] {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// HasFile reports whether a share holds a file with the given prefix.
func (s *Samba) HasFile(serverName, share, prefix string) bool {
	for f := range s.server(serverName).files[share] {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}
