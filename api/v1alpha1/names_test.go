package v1alpha1

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestNames(t *testing.T) {
	if got := SmbMountName("node-1", "storagebox-library-data"); got != "node-1.storagebox-library-data" {
		t.Fatalf("SmbMountName = %q", got)
	}
	if got := RemediationName("n1", time.Unix(1760000000, 0)); got != "n1-1760000000" {
		t.Fatalf("RemediationName = %q", got)
	}
	if got := HostTaskName("n1-1760000000", StageReleasingMounts, 2); got != "n1-1760000000-releasing-mounts-2" {
		t.Fatalf("HostTaskName = %q", got)
	}
	q := SmbQuarantineName("n1", "storagebox-x-20261011T070809Z")
	if q != "n1.storagebox-x-20261011t070809z" {
		t.Fatalf("SmbQuarantineName = %q", q)
	}
	for _, n := range []string{q, HostTaskName("r", StageAwaitingUnstageCompletion, 1)} {
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			t.Fatalf("%q invalid: %v", n, errs)
		}
	}
}

func TestLongNamesAreBoundedAndDistinct(t *testing.T) {
	node := strings.Repeat("n", 120)
	a := SmbMountName(node, strings.Repeat("p", 200)+"a")
	b := SmbMountName(node, strings.Repeat("p", 200)+"b")
	if len(a) > MaxNameLength || len(b) > MaxNameLength {
		t.Fatalf("too long: %d %d", len(a), len(b))
	}
	if a == b {
		t.Fatal("hash suffix must distinguish truncated names")
	}
	if errs := validation.IsDNS1123Subdomain(a); len(errs) > 0 {
		t.Fatalf("invalid: %v", errs)
	}
	if SmbMountName(node, strings.Repeat("p", 200)+"a") != a {
		t.Fatal("not deterministic")
	}
}

func TestStageTerminal(t *testing.T) {
	if !StageDone.Terminal() || StageFinalizing.Terminal() {
		t.Fatal("only done is terminal")
	}
}
