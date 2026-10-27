// Package fakeinfra groups the fake infrastructure the real backend runs
// against in dev mode: hub (the hub's Kubernetes API for ManagedClusters,
// LLMInferenceService / Placement / ManifestWorkReplicaSet for Models,
// MaaS Subscription/AuthPolicy/ModelRef CRs, and SelfSubjectAccessReview),
// keycloak (Keycloak's token endpoint and Organizations Admin API) and
// devserve (TLS listener and kubeconfig).
// Its tests run the backend's real servers and clients against the fakes.
//
// draft is different: it is not infrastructure but the console's fake of the
// services this backend does not implement yet (ai_grid.draft.v1, the
// console's draft proto), with its own state. Each of its areas is deleted
// when the real implementation lands; see docs/draft/README.md.
package fakeinfra
