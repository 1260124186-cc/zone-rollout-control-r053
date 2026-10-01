package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

// actorRole extracts the optional caller role used for freeze exceptions.
func actorRole(r *http.Request) string {
	return domain.NormalizeActorRole(r.Header.Get("X-Actor-Role"))
}

func (s *Server) createFreezePolicy(w http.ResponseWriter, r *http.Request) {
	var input domain.FreezePolicyInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	policy, err := s.freezes.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, policy)
}

func (s *Server) listFreezePolicies(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	activeOnly := false
	if raw := strings.TrimSpace(r.URL.Query().Get("active")); raw != "" {
		switch raw {
		case "true":
			activeOnly = true
		case "false":
		default:
			sendError(w, r, domain.Validation("active must be true or false", map[string]any{"active": raw}))
			return
		}
	}
	zone := strings.TrimSpace(r.URL.Query().Get("zone"))
	if zone != "" && zone != domain.FreezeWildcardZone {
		zone = domain.NormalizePlanInput(domain.PlanCreateInput{Zone: zone}).Zone
	}
	page, err := s.freezes.List(r.Context(), zone, activeOnly, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getFreezePolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := s.freezes.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, policy)
}

func (s *Server) updateFreezePolicy(w http.ResponseWriter, r *http.Request) {
	var input domain.FreezePolicyUpdateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	policy, err := s.freezes.Update(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, policy)
}

func (s *Server) revokeFreezePolicy(w http.ResponseWriter, r *http.Request) {
	var input domain.FreezeRevokeInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	policy, err := s.freezes.Revoke(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, policy)
}

func (s *Server) listFreezeInterceptions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	page, err := s.freezes.ListInterceptions(
		r.Context(),
		strings.TrimSpace(r.URL.Query().Get("zone")),
		strings.TrimSpace(r.URL.Query().Get("policy_id")),
		limit,
		r.URL.Query().Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}
