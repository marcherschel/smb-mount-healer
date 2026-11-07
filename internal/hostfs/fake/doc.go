// Package fake provides an in-memory, scriptable hostfs (MountTable,
// DirInspector, Prober, Mutator, QuarantineInspector) for unit tests and
// internal/sim. NewHost wires all five over shared state: unmounting
// through the Mutator changes the MountTable, quarantining empties the
// source Dir and adds a folder to the QuarantineInspector. All types are
// safe for concurrent use.
package fake
