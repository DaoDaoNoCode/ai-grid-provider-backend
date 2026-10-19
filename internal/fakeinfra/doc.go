// Package fakeinfra groups the fake infrastructure the real backend runs
// against in dev mode: hub (the hub's Kubernetes API for ManagedClusters and
// SelfSubjectAccessReview), keycloak (Keycloak's token endpoint and
// Organizations Admin API) and devserve (TLS listener and kubeconfig).
// Its tests run the backend's real servers and clients against the fakes.
package fakeinfra
