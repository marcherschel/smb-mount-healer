package hostfs

import "testing"

func TestLookupLastEntryWins(t *testing.T) {
	gm := "/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/abc/globalmount"
	s := MountSnapshot{Entries: []MountEntry{
		{ID: 1, MountPoint: "/"},
		{ID: 2, MountPoint: gm, FSType: "cifs"},
		{ID: 3, MountPoint: gm, FSType: "xfs"}, // overmount (2026-08-29 incident)
	}}
	e, ok := s.Lookup(gm)
	if !ok || e.FSType != "xfs" || e.ID != 3 {
		t.Fatalf("Lookup = %+v, %v; want the xfs overmount", e, ok)
	}
	if !s.IsMountpoint(gm) {
		t.Fatal("IsMountpoint = false")
	}
	if s.IsMountpoint(gm + "/x") {
		t.Fatal("IsMountpoint(child) = true")
	}
}
