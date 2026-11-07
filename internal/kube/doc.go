// Package kube contains the control plane's cluster adapters: owner
// resolution (Pod -> ReplicaSet -> Deployment, Pod -> StatefulSet; ReplicaSets
// read live), the scale subresource, driver pod lookup and deletion with a
// UID precondition, and Events (events.k8s.io recorder from
// mgr.GetEventRecorder). NewScheme is shared by both modes and tests.
package kube
