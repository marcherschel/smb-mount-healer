package fake

import (
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Host bundles the fakes over one shared in-memory node.
type Host struct {
	Mounts     *MountTable
	Dirs       *Dirs
	Prober     *Prober
	Mutator    *Mutator
	Quarantine *QuarantineInspector
}

// NewHost returns a Host for layout whose quarantine folders live under
// quarantineDir and whose healthy fstypes are expectedFSTypes.
func NewHost(layout kubelet.Layout, quarantineDir string, expectedFSTypes ...string) *Host {
	h := &Host{Mounts: &MountTable{}}
	h.Dirs = &Dirs{Layout: layout, mounts: h.Mounts}
	h.Prober = &Prober{ExpectedFSTypes: expectedFSTypes}
	h.Quarantine = &QuarantineInspector{Dir: quarantineDir}
	h.Mutator = &Mutator{mounts: h.Mounts, dirs: h.Dirs, quarantine: h.Quarantine}
	return h
}

var (
	_ hostfs.MountTable          = (*MountTable)(nil)
	_ hostfs.DirInspector        = (*Dirs)(nil)
	_ hostfs.Prober              = (*Prober)(nil)
	_ hostfs.Mutator             = (*Mutator)(nil)
	_ hostfs.QuarantineInspector = (*QuarantineInspector)(nil)
)
