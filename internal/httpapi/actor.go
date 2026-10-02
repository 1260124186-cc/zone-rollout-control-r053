package httpapi

import (
	"net/http"
	"strings"
)

// actorRoles extracts the caller's roles from the X-Operator-Roles header.
// Roles are comma-separated, case-insensitive and matched by exact name
// against each freeze's allowed_roles list.
func actorRoles(r *http.Request) []string {
	raw := strings.TrimSpace(r.Header.Get("X-Operator-Roles"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	roles := make([]string, 0, len(parts))
	for _, part := range parts {
		if role := strings.ToLower(strings.TrimSpace(part)); role != "" {
			roles = append(roles, role)
		}
	}
	return roles
}
