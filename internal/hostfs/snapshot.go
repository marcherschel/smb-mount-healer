package hostfs

// Lookup returns the mount entry visible at path. When several entries exist
// for the same mountpoint (overmounts), the last one in mountinfo order wins.
func (s MountSnapshot) Lookup(path string) (MountEntry, bool) {
	for i := len(s.Entries) - 1; i >= 0; i-- {
		if s.Entries[i].MountPoint == path {
			return s.Entries[i], true
		}
	}
	return MountEntry{}, false
}

// IsMountpoint reports whether path is a mountpoint in this snapshot.
func (s MountSnapshot) IsMountpoint(path string) bool {
	_, ok := s.Lookup(path)
	return ok
}
