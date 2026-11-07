//go:build e2e

package framework

import (
	"fmt"
	"strings"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

// Chaos drives the privileged chaos DaemonSet (manifests/chaos): one pod per
// node with the host's /var/lib/kubelet mounted Bidirectional, alpine with
// cifs-utils and util-linux. Mounts and unmounts made here are host mounts.
type Chaos struct{ f *F }

const chaosSelector = NameLabel + "=chaos"

// Pod returns the ready chaos pod on node.
func (c *Chaos) Pod(node string) *corev1.Pod {
	c.f.T.Helper()
	return c.f.ReadyPod(ChaosNamespace, chaosSelector, node)
}

// Exec runs cmd in the chaos pod on node.
func (c *Chaos) Exec(node string, cmd ...string) (ExecResult, error) {
	c.f.T.Helper()
	p := c.Pod(node)
	return c.f.ExecPod(ChaosNamespace, p.Name, "chaos", cmd...)
}

// Sh runs a shell script in the chaos pod on node and fails the test on a
// non-zero exit. Returns stdout.
func (c *Chaos) Sh(node, script string) string {
	c.f.T.Helper()
	res, err := c.Exec(node, "sh", "-c", script)
	c.f.G.Expect(err).NotTo(gomega.HaveOccurred())
	return res.Stdout
}

// MountInfo returns fstype and super options of the topmost mount exactly at
// path on node, or ("", "") if path is not a mountpoint.
func (c *Chaos) MountInfo(node, path string) (fstype, superOptions string) {
	c.f.T.Helper()
	out := c.Sh(node, fmt.Sprintf("findmnt -n -r -o FSTYPE,FS-OPTIONS --mountpoint %s | tail -n 1 || true", ShellQuote(path)))
	fields := strings.Fields(strings.TrimSpace(out))
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	default:
		return fields[0], fields[1]
	}
}

// LazyUnmount runs `umount -l path` on node (the WRONG_FSTYPE / detached
// globalmount injection).
func (c *Chaos) LazyUnmount(node, path string) {
	c.f.T.Helper()
	c.Sh(node, "umount -l "+ShellQuote(path))
}

// MountCIFS mounts //host/share onto path on node with the e2e credentials
// and the fixture mount options (S8: cifs-mount samba-landmine onto a globalmount).
func (c *Chaos) MountCIFS(node, unc, path string) {
	c.f.T.Helper()
	user, pass := c.f.Fixtures.Credentials()
	opts := "username=" + user + ",password=" + pass + "," + strings.Join(MountOptions, ",")
	c.Sh(node, fmt.Sprintf("mkdir -p %s && mount -t cifs %s %s -o %s",
		ShellQuote(path), ShellQuote(unc), ShellQuote(path), ShellQuote(opts)))
}

// WriteFile writes content to path on node (creating parent dirs), e.g. to
// seed shadow files in an unmounted globalmount or an orphaned pod dir.
func (c *Chaos) WriteFile(node, path, content string) {
	c.f.T.Helper()
	c.Sh(node, fmt.Sprintf("mkdir -p \"$(dirname %s)\" && printf %%s %s > %s",
		ShellQuote(path), ShellQuote(content), ShellQuote(path)))
}
