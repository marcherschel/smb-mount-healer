//go:build e2e

package framework

import (
	"bytes"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
)

// ExecResult is the outcome of a command run in a pod.
type ExecResult struct {
	Stdout, Stderr string
}

// ExecError carries the output of a failed command.
type ExecError struct {
	Pod    string
	Cmd    []string
	Result ExecResult
	Err    error
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("exec %v in %s: %v (stdout=%q stderr=%q)", e.Cmd, e.Pod, e.Err, e.Result.Stdout, e.Result.Stderr)
}

func (e *ExecError) Unwrap() error { return e.Err }

// ExecPod runs cmd in container of pod ns/name through the API server
// (WebSocket, falling back to SPDY like kubectl) and returns its output. A
// non-zero exit status is an *ExecError.
func (f *F) ExecPod(ns, name, container string, cmd ...string) (ExecResult, error) {
	req := f.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	ws, err := remotecommand.NewWebSocketExecutor(f.Config, "GET", req.URL().String())
	if err != nil {
		return ExecResult{}, err
	}
	spdy, err := remotecommand.NewSPDYExecutor(f.Config, "POST", req.URL())
	if err != nil {
		return ExecResult{}, err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return ExecResult{}, err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(f.Ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, &ExecError{Pod: ns + "/" + name, Cmd: cmd, Result: res, Err: err}
	}
	return res, nil
}

// Sh is a convenience for ExecPod(..., "sh", "-c", script).
func (f *F) Sh(ns, name, container, script string) (ExecResult, error) {
	return f.ExecPod(ns, name, container, "sh", "-c", script)
}

// ShellQuote single-quotes s for use in a /bin/sh command line.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
