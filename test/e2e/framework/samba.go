//go:build e2e

package framework

import (
	"fmt"
	"strings"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Samba controls one samba Deployment in smbmh-samba ("samba" or
// "samba-landmine"). Share data lives under /shares/<share> in the pod.
type Samba struct {
	f    *F
	Name string
}

func (s *Samba) selector() string { return NameLabel + "=" + s.Name }

// Pod returns the ready samba pod.
func (s *Samba) Pod() *corev1.Pod {
	s.f.T.Helper()
	return s.f.ReadyPod(SambaNamespace, s.selector(), "")
}

func (s *Samba) sh(script string) (ExecResult, error) {
	s.f.T.Helper()
	return s.f.Sh(SambaNamespace, s.Pod().Name, "samba", script)
}

// Pause freezes every smbd process (SIGSTOP; TCP connects still complete in
// the kernel backlog, I/O hangs) and resumes them after d from inside the pod,
// so the server comes back even if the test dies. Resume is registered as a
// cleanup as well. d must be at least one second.
func (s *Samba) Pause(d time.Duration) {
	s.f.T.Helper()
	secs := int(d.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	_, err := s.sh(fmt.Sprintf(
		"pkill -STOP smbd && (setsid sh -c 'sleep %d; pkill -CONT smbd' </dev/null >/dev/null 2>&1 &)", secs))
	s.f.G.Expect(err).NotTo(gomega.HaveOccurred())
	s.f.T.Cleanup(s.Resume)
}

// Resume sends SIGCONT to every smbd process (idempotent).
func (s *Samba) Resume() {
	s.f.T.Helper()
	_, err := s.sh("pkill -CONT smbd || true")
	s.f.G.Expect(err).NotTo(gomega.HaveOccurred())
}

// Restart deletes the samba pod and waits for a ready replacement (new pod
// IP behind the headless Service, S13). Returns the new pod.
func (s *Samba) Restart() *corev1.Pod {
	s.f.T.Helper()
	old := s.Pod()
	s.f.G.Expect(s.f.Kube.CoreV1().Pods(SambaNamespace).Delete(s.f.Ctx, old.Name, metav1.DeleteOptions{})).To(gomega.Succeed())
	var p *corev1.Pod
	s.f.G.Eventually(func(g gomega.Gomega) {
		var err error
		p, err = s.f.readyPod(SambaNamespace, s.selector(), "")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(p.UID).NotTo(gomega.Equal(old.UID))
	}).WithTimeout(3 * time.Minute).Should(gomega.Succeed())
	return p
}

// Scale sets the samba Deployment's replicas (S14 outage; S8 deletes the
// landmine). Registers no cleanup: callers restore it.
func (s *Samba) Scale(replicas int32) {
	s.f.T.Helper()
	scaleDeployment(s.f, SambaNamespace, s.Name, replicas)
}

// Cat returns the content of share/file as seen by the server.
func (s *Samba) Cat(share, file string) (string, error) {
	res, err := s.sh("cat " + ShellQuote("/shares/"+share+"/"+file))
	return res.Stdout, err
}

// Ls lists the entries of share (names only, sorted by ls).
func (s *Samba) Ls(share string) ([]string, error) {
	res, err := s.sh("ls -1A " + ShellQuote("/shares/"+share))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}
