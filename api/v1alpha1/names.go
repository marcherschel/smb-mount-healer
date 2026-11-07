package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// MaxNameLength is the maximum length of a cluster-scoped object name (DNS subdomain).
const MaxNameLength = 253

const hashSuffixLen = 10

// SmbMountName returns "<node>.<pv>", shortened with a hash suffix if it exceeds 253 characters.
func SmbMountName(node, pv string) string { return boundedName(node + "." + pv) }

// SmbQuarantineName returns "<node>.<folder>", shortened with a hash suffix if too long.
// folder is the basename of a quarantine folder created by the healer ("<pv>-<UTC>").
func SmbQuarantineName(node, folder string) string {
	return boundedName(node + "." + strings.ToLower(folder))
}

// RemediationName returns "<node>-<unix seconds>".
func RemediationName(node string, t time.Time) string {
	return boundedName(node + "-" + strconv.FormatInt(t.Unix(), 10))
}

// HostTaskName returns "<remediation>-<stage>-<seq>" with "_" mapped to "-", shortened if needed.
func HostTaskName(remediation string, stage Stage, seq int) string {
	return boundedName(remediation + "-" + strings.ReplaceAll(string(stage), "_", "-") + "-" + strconv.Itoa(seq))
}

// boundedName keeps names that fit unchanged; longer names are truncated and
// suffixed with "-" + the first 10 hex chars of sha256(full name).
func boundedName(full string) string {
	if len(full) <= MaxNameLength {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(sum[:])[:hashSuffixLen]
	prefix := strings.TrimRight(full[:MaxNameLength-hashSuffixLen-1], ".-")
	return prefix + "-" + suffix
}
