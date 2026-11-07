package kubelet

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// QuarantineTimeFormat is the UTC timestamp suffix of quarantine folders.
const QuarantineTimeFormat = "20060102T150405Z"

// Layout describes the kubelet directory tree for one CSI driver.
type Layout struct {
	Root       string // e.g. /var/lib/kubelet
	DriverName string // e.g. smb.csi.k8s.io
}

// StagingRoot is <root>/plugins/kubernetes.io/csi/<driver>.
func (l Layout) StagingRoot() string {
	return filepath.Join(l.Root, "plugins", "kubernetes.io", "csi", l.DriverName)
}

// GlobalmountHash is sha256hex(volumeHandle).
func GlobalmountHash(volumeHandle string) string {
	sum := sha256.Sum256([]byte(volumeHandle))
	return hex.EncodeToString(sum[:])
}

// GlobalmountPath is <staging root>/<sha256hex(volumeHandle)>/globalmount.
func (l Layout) GlobalmountPath(volumeHandle string) string {
	return filepath.Join(l.StagingRoot(), GlobalmountHash(volumeHandle), "globalmount")
}

// PodsRoot is <root>/pods.
func (l Layout) PodsRoot() string { return filepath.Join(l.Root, "pods") }

// PodVolumeMountPath is <root>/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount.
func (l Layout) PodVolumeMountPath(uid types.UID, pv string) string {
	return filepath.Join(l.PodsRoot(), string(uid), "volumes", "kubernetes.io~csi", pv, "mount")
}

// QuarantineFolderName returns "<pv>-<UTC YYYYMMDDTHHMMSSZ>".
func QuarantineFolderName(pv string, t time.Time) string {
	return pv + "-" + t.UTC().Format(QuarantineTimeFormat)
}

// QuarantineFolderNameN returns QuarantineFolderName for n <= 1 and
// "<pv>-<UTC>-<n>" otherwise. The suffix disambiguates two quarantines of the
// same PV within one second; ParseQuarantineFolderName accepts it.
func QuarantineFolderNameN(pv string, t time.Time, n int) string {
	base := QuarantineFolderName(pv, t)
	if n <= 1 {
		return base
	}
	return base + "-" + strconv.Itoa(n)
}

var quarantineFolderRE = regexp.MustCompile(`^(.+)-(\d{8}T\d{6}Z)(?:-([1-9]\d{0,5}))?$`)

// ParseQuarantineFolderName parses "<pv>-<UTC>" (also folders left by the
// bash healer) and the collision form "<pv>-<UTC>-<n>".
func ParseQuarantineFolderName(name string) (pv string, t time.Time, ok bool) {
	m := quarantineFolderRE.FindStringSubmatch(name)
	if m == nil {
		return "", time.Time{}, false
	}
	ts, err := time.Parse(QuarantineTimeFormat, m[2])
	if err != nil {
		return "", time.Time{}, false
	}
	return m[1], ts.UTC(), true
}
