package controlplane_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/reference"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/controlplane"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
)

type recorded struct {
	ref                             *corev1.ObjectReference
	eventType, reason, action, note string
}

type captureRecorder struct {
	scheme *runtime.Scheme
	got    []recorded
	t      *testing.T
}

func (r *captureRecorder) Eventf(regarding, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	ref, err := reference.GetReference(r.scheme, regarding)
	if err != nil {
		r.t.Fatalf("GetReference: %v", err)
	}
	if note != "%s" || len(args) != 1 {
		r.t.Fatalf("message must be passed verbatim, got format %q", note)
	}
	r.got = append(r.got, recorded{ref, eventType, reason, action, args[0].(string)})
}

func TestEmitEvent(t *testing.T) {
	s, err := kube.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	rec := &captureRecorder{scheme: s, t: t}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	kc, err := kube.NewClientCluster(kube.ClientOptions{Client: c, Recorder: rec, DriverNamespace: "kube-system",
		DriverSelector: labels.SelectorFromSet(labels.Set{"app": "csi-smb-node"})})
	if err != nil {
		t.Fatal(err)
	}
	e, err := controlplane.NewExecutor(controlplane.ExecutorOptions{Cluster: kc, Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		ref        command.ObjectRef
		apiVersion string
	}{
		{command.ObjectRef{Kind: "SmbMountRemediation", Name: "node-a-1", UID: "u1"}, v1.GroupVersion.String()},
		{command.ObjectRef{Kind: "Pod", Namespace: "kube-system", Name: "csi-smb-node-x", UID: "u2"}, "v1"},
		{command.ObjectRef{Kind: "StatefulSet", Namespace: "media", Name: "db", UID: "u3"}, "apps/v1"},
	} {
		rec.got = nil
		err := e.ExecuteCluster(ctx, command.EmitEvent{Regarding: tc.ref, Type: command.EventWarning, Reason: "R", Action: "A", Message: "50% done"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rec.got) != 1 {
			t.Fatalf("%s: %d events", tc.ref.Kind, len(rec.got))
		}
		g := rec.got[0]
		want := corev1.ObjectReference{Kind: tc.ref.Kind, APIVersion: tc.apiVersion, Namespace: tc.ref.Namespace, Name: tc.ref.Name, UID: tc.ref.UID}
		if *g.ref != want || g.eventType != command.EventWarning || g.reason != "R" || g.action != "A" || g.note != "50% done" {
			t.Fatalf("%s: got %+v %+v", tc.ref.Kind, *g.ref, g)
		}
	}
	if err := e.ExecuteCluster(ctx, command.EmitEvent{Regarding: command.ObjectRef{Kind: "Secret", Name: "x"}, Type: command.EventNormal, Reason: "R"}); err == nil {
		t.Fatal("unsupported kind must be rejected")
	}
}
