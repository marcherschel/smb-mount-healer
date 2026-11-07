package remediation

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

var updateDocs = flag.Bool("update-docs", false, "rewrite docs/state-machine.md from the transition table")

const docPath = "../../docs/state-machine.md"

func stageName(s v1alpha1.Stage) string {
	if s == "" {
		return "[*]"
	}
	return string(s)
}

func renderMermaid(b *strings.Builder, machine v1alpha1.RemediationType) {
	type edge struct{ from, to string }
	var order []edge
	labels := map[edge][]string{}
	for _, r := range rules {
		if r.Machine != machine {
			continue
		}
		for _, f := range r.From {
			e := edge{stageName(f), stageName(r.To)}
			if _, ok := labels[e]; !ok {
				order = append(order, e)
			}
			labels[e] = append(labels[e], r.Reasons...)
		}
	}
	b.WriteString("```mermaid\nstateDiagram-v2\n")
	for _, e := range order {
		fmt.Fprintf(b, "    %s --> %s: %s\n", e.from, e.to, strings.Join(labels[e], ", "))
	}
	b.WriteString("    done --> [*]\n```\n\n")
}

func renderTable(b *strings.Builder, machine v1alpha1.RemediationType) {
	b.WriteString("| From | To | Reasons | Guard | Actions |\n|---|---|---|---|---|\n")
	for _, r := range rules {
		if r.Machine != machine {
			continue
		}
		var from []string
		for _, f := range r.From {
			if f == "" {
				from = append(from, "(new)")
			} else {
				from = append(from, "`"+string(f)+"`")
			}
		}
		fromCell := strings.Join(from, ", ")
		if len(r.From) > 4 {
			fromCell = "any non-terminal"
		}
		actions := r.Actions
		if actions == "" {
			actions = "–"
		}
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s |\n", fromCell, r.To,
			"`"+strings.Join(r.Reasons, "`, `")+"`", r.Guard, actions)
	}
	b.WriteString("\n")
}

func renderDoc() string {
	var b strings.Builder
	b.WriteString(`# Remediation state machines

<!-- Code generated from internal/remediation/table.go by
     "go generate ./internal/remediation"; DO NOT EDIT. -->

Every stage change of an ` + "`SmbMountRemediation`" + ` is one row of the tables
below; ` + "`remediation.Step`" + ` never produces any other transition (enforced by
the package tests). Wait guards poll every ` + "`POLL_INTERVAL`" + ` until their
deadline, which is an absolute timestamp in ` + "`status.deadline`" + `. In dry run
every wait guard passes immediately, host tasks are created with
` + "`dryRun: true`" + ` (only their existence is awaited), no workload is scaled and
no pod is deleted; the outcome is ` + "`dry_run`" + `.

The control plane writes ` + "`Result.Status`" + ` before executing
` + "`Result.Commands`" + ` (write-ahead); every command is idempotent.

`)
	for _, m := range []v1alpha1.RemediationType{v1alpha1.RemediationTypeWorkload, v1alpha1.RemediationTypeSweep} {
		fmt.Fprintf(&b, "## %s\n\n", m)
		renderMermaid(&b, m)
		renderTable(&b, m)
	}
	return b.String()
}

func TestStateMachineDocUpToDate(t *testing.T) {
	want := renderDoc()
	path := filepath.FromSlash(docPath)
	if *updateDocs {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go generate ./internal/remediation)", err)
	}
	if string(got) != want {
		t.Fatalf("%s is stale; run go generate ./internal/remediation", docPath)
	}
}
