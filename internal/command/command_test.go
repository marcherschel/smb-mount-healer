package command

import (
	"testing"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

func TestFromHostAction(t *testing.T) {
	tests := []struct {
		in      v1alpha1.HostAction
		want    HostCommand
		wantErr bool
	}{
		{in: v1alpha1.HostAction{ID: "a", Type: v1alpha1.HostActionForceUnmount, Path: "/p", OnlyIf: v1alpha1.OnlyIfNotHealthyMount},
			want: ForceUnmount{ActionID: "a", Path: "/p", OnlyIf: v1alpha1.OnlyIfNotHealthyMount}},
		{in: v1alpha1.HostAction{ID: "b", Type: v1alpha1.HostActionQuarantine, Path: "/p", PV: "pv", OnlyIf: v1alpha1.OnlyIfNotMountpoint},
			want: Quarantine{ActionID: "b", Path: "/p", PV: "pv", OnlyIf: v1alpha1.OnlyIfNotMountpoint}},
		{in: v1alpha1.HostAction{ID: "c", Type: v1alpha1.HostActionProbeNow, Path: "/p"},
			want: ProbeNow{ActionID: "c", Path: "/p"}},
		{in: v1alpha1.HostAction{ID: "d", Type: v1alpha1.HostActionQuarantine, Path: "/p"}, wantErr: true},
		{in: v1alpha1.HostAction{ID: "e", Type: "Bogus", Path: "/p"}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := FromHostAction(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: err = %v", tt.in.ID, err)
		}
		if got != tt.want {
			t.Fatalf("%s: got %#v, want %#v", tt.in.ID, got, tt.want)
		}
		if got != nil && got.ID() != tt.in.ID {
			t.Fatalf("%s: ID() = %q", tt.in.ID, got.ID())
		}
	}
}

func TestKeysStable(t *testing.T) {
	c := ScaleWorkload{Workload: v1alpha1.WorkloadRef{Kind: v1alpha1.WorkloadKindDeployment, Namespace: "ns", Name: "app"}, Replicas: 0}
	if c.Key() != "scale/Deployment/ns/app/0" {
		t.Fatalf("key = %q", c.Key())
	}
}
