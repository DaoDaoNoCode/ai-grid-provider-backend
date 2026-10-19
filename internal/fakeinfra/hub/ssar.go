package hub

// Ported from the console's fake hub (ai-grid-cloud/ai-grid-provider-console
// 537df38, bff/internal/fakebackend/hub.go), so the console BFF's
// isProviderAdmin check gets the same answers from this hub as from the
// console's own fake. One difference: the decision is Hub.allowed, shared
// with the managedclusters authorizer, so the backend's ServiceAccount is
// allowed here too when defaultAllowed is false, and the review names the
// ServiceAccount as the user for its token.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

type accessReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		ResourceAttributes map[string]string `json:"resourceAttributes"`
	} `json:"spec"`
	Status struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason,omitempty"`
	} `json:"status"`
}

// selfSubjectAccessReview answers the way the real API server does,
// including the 401 for a bad token. It does not look at the requested verb
// or resource, so every access check gets the same answer for a given token.
func (h *Hub) selfSubjectAccessReview(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || slices.Contains(h.access.RejectedTokens, token) {
		writeConsoleStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	var review accessReview
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&review); err != nil {
		writeConsoleStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	allowed := h.allowed(token)
	review.Status.Allowed = allowed
	attrs := review.Spec.ResourceAttributes
	if allowed {
		review.Status.Reason = fmt.Sprintf("RBAC: allowed by ClusterRoleBinding %q of ClusterRole %q to User %q", "ai-grid-provider-admins", "ai-grid-provider-admin", h.username(token))
	}
	h.logger.Info("access review", "verb", attrs["verb"], "resource", attrs["resource"], "group", attrs["group"], "allowed", allowed)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(review)
}

// writeConsoleStatus is the console fake's writeStatus: a metav1.Status the
// way the API server writes one.
func writeConsoleStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
		"status": "Failure", "message": message, "reason": reason, "code": code,
	})
}
