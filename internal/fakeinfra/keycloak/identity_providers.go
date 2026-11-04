package keycloak

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type identityProvider struct {
	Alias       string            `json:"alias"`
	DisplayName string            `json:"displayName,omitempty"`
	ProviderID  string            `json:"providerId"`
	Enabled     bool              `json:"enabled"`
	Config      map[string]string `json:"config"`
}

type identityProviderMapper struct {
	ID                     string            `json:"id,omitempty"`
	Name                   string            `json:"name"`
	IdentityProviderAlias  string            `json:"identityProviderAlias"`
	IdentityProviderMapper string            `json:"identityProviderMapper"`
	Config                 map[string]string `json:"config"`
}

type orgIdPLink struct {
	AutoMembership bool   `json:"autoMembership"`
	MembershipType string `json:"membershipType"`
}

type orgIdPLinkState struct {
	alias string
	link  orgIdPLink
}

func (s *Server) createIdP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var idp identityProvider
	if err := json.Unmarshal(body, &idp); err != nil || idp.Alias == "" || idp.ProviderID == "" {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid identity provider representation")
		return
	}
	if _, ok := s.idps[idp.Alias]; ok {
		writeErrorMessage(w, http.StatusConflict, "Identity Provider with alias already exists")
		return
	}
	if idp.Config == nil {
		idp.Config = map[string]string{}
	}
	s.idps[idp.Alias] = &idp
	s.mappers[idp.Alias] = nil
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) getIdP(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	idp, ok := s.idps[alias]
	if !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	writeJSON(w, http.StatusOK, redactIdP(*idp))
}

func (s *Server) deleteIdP(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	delete(s.idps, alias)
	delete(s.mappers, alias)
	for orgID, link := range s.orgIdPs {
		if link.alias == alias {
			delete(s.orgIdPs, orgID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) linkOrgIdP(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	if _, ok := s.orgs[orgID]; !ok {
		writeError(w, http.StatusNotFound, "HTTP 404 Not Found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var alias string
	if err := json.Unmarshal(body, &alias); err != nil || alias == "" {
		writeErrorMessage(w, http.StatusBadRequest, "Identity provider not found with the given alias")
		return
	}
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusBadRequest, "Identity provider not found with the given alias")
		return
	}
	if _, ok := s.orgIdPs[orgID]; ok {
		writeErrorMessage(w, http.StatusConflict, "Identity provider already associated to the organization")
		return
	}
	s.orgIdPs[orgID] = orgIdPLinkState{alias: alias, link: orgIdPLink{}}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) configureOrgIdP(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("id")
	alias := r.PathValue("alias")
	state, ok := s.orgIdPs[orgID]
	if !ok || state.alias != alias {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not associated with the organization")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var link orgIdPLink
	if err := json.Unmarshal(body, &link); err != nil {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid membership type: ")
		return
	}
	if link.MembershipType == "MANAGED" && !link.AutoMembership {
		writeErrorMessage(w, http.StatusBadRequest, "Auto-membership must be enabled when membership type is MANAGED")
		return
	}
	if link.MembershipType != "MANAGED" && link.MembershipType != "UNMANAGED" {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid membership type: "+link.MembershipType)
		return
	}
	state.link = link
	s.orgIdPs[orgID] = state
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIdPMappers(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	out := s.mappers[alias]
	if out == nil {
		out = []*identityProviderMapper{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createIdPMapper(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if _, ok := s.idps[alias]; !ok {
		writeErrorMessage(w, http.StatusNotFound, "Identity provider not found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var mapper identityProviderMapper
	if err := json.Unmarshal(body, &mapper); err != nil || mapper.Name == "" {
		writeErrorMessage(w, http.StatusBadRequest, "Invalid identity provider mapper")
		return
	}
	mapper.ID = newMapperID()
	mapper.IdentityProviderAlias = alias
	s.mappers[alias] = append(s.mappers[alias], &mapper)
	writeJSON(w, http.StatusCreated, mapper)
}

func (s *Server) updateIdPMapper(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	id := r.PathValue("mapperId")
	list := s.mappers[alias]
	body, _ := io.ReadAll(r.Body)
	for i, m := range list {
		if m.ID != id {
			continue
		}
		var mapper identityProviderMapper
		if err := json.Unmarshal(body, &mapper); err != nil {
			writeErrorMessage(w, http.StatusBadRequest, "Invalid identity provider mapper")
			return
		}
		mapper.ID = id
		mapper.IdentityProviderAlias = alias
		list[i] = &mapper
		s.mappers[alias] = list
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeErrorMessage(w, http.StatusNotFound, "Identity provider mapper not found")
}

func redactIdP(idp identityProvider) identityProvider {
	out := idp
	if out.Config != nil {
		cfg := make(map[string]string, len(out.Config))
		for k, v := range out.Config {
			if strings.EqualFold(k, "clientSecret") {
				cfg[k] = ""
				continue
			}
			cfg[k] = v
		}
		out.Config = cfg
	}
	return out
}

func newMapperID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
