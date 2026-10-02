package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) registerFreeze(w http.ResponseWriter, r *http.Request) {
	var input domain.FreezeCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.freezes.Register(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, result)
}

func (s *Server) getFreeze(w http.ResponseWriter, r *http.Request) {
	result, err := s.freezes.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) listFreezes(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		sendError(w, r, err)
		return
	}
	status := domain.FreezeStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	page, err := s.freezes.List(
		r.Context(),
		status,
		r.URL.Query().Get("zone"),
		limit,
		r.URL.Query().Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) updateFreeze(w http.ResponseWriter, r *http.Request) {
	input, err := decodeFreezeUpdate(w, r)
	if err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.freezes.Update(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) revokeFreeze(w http.ResponseWriter, r *http.Request) {
	var input domain.FreezeRevokeInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.freezes.Revoke(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) listFreezeBlocks(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		sendError(w, r, err)
		return
	}
	query := r.URL.Query()
	page, err := s.freezes.ListBlocks(
		r.Context(),
		strings.TrimSpace(query.Get("freeze_id")),
		strings.TrimSpace(query.Get("plan_id")),
		domain.FreezeScope(strings.TrimSpace(query.Get("scope"))),
		limit,
		query.Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func parseLimit(r *http.Request) (int, error) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, domain.Validation("limit must be an integer", map[string]any{"limit": raw})
		}
		limit = parsed
	}
	return limit, nil
}

// decodeFreezeUpdate decodes the update body while distinguishing an
// absent ends_at from an explicit JSON null (which clears the end time).
func decodeFreezeUpdate(w http.ResponseWriter, r *http.Request) (domain.FreezeUpdateInput, error) {
	var input domain.FreezeUpdateInput
	r.Body = http.MaxBytesReader(w, r.Body, requestLimit)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return input, domain.Validation("request body could not be read", map[string]any{"reason": err.Error()})
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		return input, domain.Validation("request body must contain valid JSON", nil)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return input, domain.Validation("request body must contain valid JSON", map[string]any{"reason": err.Error()})
	}
	knownFields := map[string]struct{}{
		"expected_version": {},
		"scopes":           {},
		"starts_at":        {},
		"ends_at":          {},
		"reason":           {},
		"allowed_roles":    {},
		"updated_by":       {},
	}
	for name := range fields {
		if _, ok := knownFields[name]; !ok {
			return input, domain.Validation("request body contains an unknown field", map[string]any{
				"field": name,
			})
		}
	}
	type updateShape struct {
		ExpectedVersion *uint64   `json:"expected_version"`
		Scopes          *[]string `json:"scopes"`
		StartsAt        *string   `json:"starts_at"`
		Reason          *string   `json:"reason"`
		AllowedRoles    *[]string `json:"allowed_roles"`
		UpdatedBy       string    `json:"updated_by"`
	}
	var shaped updateShape
	if err := json.Unmarshal(payload, &shaped); err != nil {
		return input, domain.Validation("freeze update is invalid", map[string]any{"reason": err.Error()})
	}
	input.ExpectedVersion = shaped.ExpectedVersion
	input.Scopes = shaped.Scopes
	input.StartsAt = shaped.StartsAt
	input.Reason = shaped.Reason
	input.AllowedRoles = shaped.AllowedRoles
	input.UpdatedBy = shaped.UpdatedBy
	if raw, present := fields["ends_at"]; present {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "null" {
			input.ClearEndsAt = true
		} else {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return input, domain.Validation("freeze update is invalid", map[string]any{
					"ends_at": "must be an RFC3339 timestamp or null",
				})
			}
			input.EndsAt = &value
		}
	}
	return input, nil
}
