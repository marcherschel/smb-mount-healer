// Package config parses the command line and environment for each mode
// (--controlplane, --worker, hidden --probe, --version) and validates it.
// Durations accept plain seconds ("300") or Go durations ("5m").
package config
