//go:build e2e

// Package framework provides the e2e helpers: metric scraping through
// the API-server pod proxy (expfmt) with Delta, CR helpers (SmbMounts,
// Remediations, WaitRemediation, StageTrace), chaos/samba/fixture helpers and
// WaitBaseline. No sleeps: Gomega Eventually/Consistently only.
package framework
