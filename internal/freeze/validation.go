package freeze

import (
	"strings"
	"time"
	"unicode/utf8"

	"solo-0014-zone-rollout-control/internal/domain"
)

const (
	MaxFreezeReasonRunes = 500
	MaxFreezeActorRunes  = 120
	MaxFreezeNoteRunes   = 500
)

var validScopes = map[domain.FreezeScope]struct{}{
	domain.FreezeScopeRevision:   {},
	domain.FreezeScopeActivation: {},
	domain.FreezeScopeAllocation: {},
}

// PreparedCreate carries the normalized, validated values used to persist
// a freeze registration.
type PreparedCreate struct {
	Zone         string
	Scopes       []domain.FreezeScope
	StartsAt     time.Time
	EndsAt       *time.Time
	Reason       string
	AllowedRoles []string
	CreatedBy    string
}

func NormalizeRoles(roles []string) []string {
	return normalizeRoles(roles)
}

// PrepareCreate validates and normalizes freeze registration input.
func PrepareCreate(input domain.FreezeCreateInput, now time.Time) (PreparedCreate, error) {
	details := make(map[string]any)

	zone := canonicalName(input.Zone)
	if zone != "" {
		if err := validateZone(zone); err != nil {
			details["zone"] = err.Error()
		}
	}

	scopes, scopeDetails := normalizeScopes(input.Scopes)
	if scopeDetails != "" {
		details["scopes"] = scopeDetails
	}

	startsAt, err := parseBoundary(input.StartsAt)
	if err != nil {
		details["starts_at"] = "must be an RFC3339 timestamp"
	}
	var endsAt *time.Time
	if strings.TrimSpace(input.EndsAt) != "" {
		parsed, err := parseBoundary(input.EndsAt)
		if err != nil {
			details["ends_at"] = "must be an RFC3339 timestamp"
		} else {
			endsAt = &parsed
		}
	}
	if err == nil && endsAt != nil && !endsAt.After(startsAt) {
		details["ends_at"] = "must be after starts_at"
	}

	reason := strings.TrimSpace(input.Reason)
	if count := utf8.RuneCountInString(reason); count < 1 || count > MaxFreezeReasonRunes {
		details["reason"] = "must contain between 1 and 500 characters"
	}

	createdBy := strings.TrimSpace(input.CreatedBy)
	if utf8.RuneCountInString(createdBy) > MaxFreezeActorRunes {
		details["created_by"] = "must contain at most 120 characters"
	}

	if len(details) > 0 {
		return PreparedCreate{}, domain.Validation("freeze input is invalid", details)
	}
	return PreparedCreate{
		Zone:         zone,
		Scopes:       scopes,
		StartsAt:     startsAt,
		EndsAt:       endsAt,
		Reason:       reason,
		AllowedRoles: NormalizeRoles(input.AllowedRoles),
		CreatedBy:    createdBy,
	}, nil
}

// PreparedUpdate describes the mutable fields selected by an update.
type PreparedUpdate struct {
	Scopes       *[]domain.FreezeScope
	StartsAt     *time.Time
	EndsAt       *time.Time // nil pointer means no change; non-nil target clears or sets
	Reason       *string
	AllowedRoles *[]string
	UpdatedBy    string
}

// PrepareUpdate validates an update request. The rawEndsAt triple is
// produced by the HTTP layer: absent means unchanged, a null clears the
// end, otherwise the value is parsed as RFC3339.
func PrepareUpdate(input domain.FreezeUpdateInput) (PreparedUpdate, error) {
	details := make(map[string]any)
	prepared := PreparedUpdate{UpdatedBy: strings.TrimSpace(input.UpdatedBy)}

	if utf8.RuneCountInString(prepared.UpdatedBy) > MaxFreezeActorRunes {
		details["updated_by"] = "must contain at most 120 characters"
	}
	if input.Scopes != nil {
		scopes, scopeDetails := normalizeScopes(*input.Scopes)
		if scopeDetails != "" {
			details["scopes"] = scopeDetails
		} else {
			prepared.Scopes = &scopes
		}
	}
	if input.StartsAt != nil {
		parsed, err := parseBoundary(*input.StartsAt)
		if err != nil {
			details["starts_at"] = "must be an RFC3339 timestamp"
		} else {
			prepared.StartsAt = &parsed
		}
	}
	if input.ClearEndsAt {
		cleared := time.Time{}
		prepared.EndsAt = &cleared
	} else if input.EndsAt != nil {
		if strings.TrimSpace(*input.EndsAt) == "" {
			details["ends_at"] = "must be an RFC3339 timestamp or null"
		} else {
			parsed, err := parseBoundary(*input.EndsAt)
			if err != nil {
				details["ends_at"] = "must be an RFC3339 timestamp or null"
			} else {
				prepared.EndsAt = &parsed
			}
		}
	}
	if input.Reason != nil {
		reason := strings.TrimSpace(*input.Reason)
		if count := utf8.RuneCountInString(reason); count < 1 || count > MaxFreezeReasonRunes {
			details["reason"] = "must contain between 1 and 500 characters"
		} else {
			prepared.Reason = &reason
		}
	}
	if input.AllowedRoles != nil {
		roles := NormalizeRoles(*input.AllowedRoles)
		prepared.AllowedRoles = &roles
	}
	if len(details) > 0 {
		return PreparedUpdate{}, domain.Validation("freeze update is invalid", details)
	}
	return prepared, nil
}

// ValidateSchedule verifies the effective start/end ordering after an
// update merges into the stored freeze.
func ValidateSchedule(startsAt time.Time, endsAt *time.Time) error {
	if endsAt != nil && !endsAt.After(startsAt) {
		return domain.Validation("freeze update is invalid", map[string]any{
			"ends_at": "must be after starts_at",
		})
	}
	return nil
}

func PrepareRevoke(input domain.FreezeRevokeInput) (string, string, error) {
	by := strings.TrimSpace(input.RevokedBy)
	note := strings.TrimSpace(input.Note)
	details := make(map[string]any)
	if utf8.RuneCountInString(by) > MaxFreezeActorRunes {
		details["revoked_by"] = "must contain at most 120 characters"
	}
	if utf8.RuneCountInString(note) > MaxFreezeNoteRunes {
		details["note"] = "must contain at most 500 characters"
	}
	if len(details) > 0 {
		return "", "", domain.Validation("freeze revocation is invalid", details)
	}
	return by, note, nil
}

func normalizeScopes(raw []string) ([]domain.FreezeScope, string) {
	if len(raw) == 0 {
		return nil, "must select at least one of revision, activation, allocation"
	}
	seen := make(map[domain.FreezeScope]struct{}, len(raw))
	scopes := make([]domain.FreezeScope, 0, len(raw))
	for _, item := range raw {
		scope := domain.FreezeScope(strings.ToLower(strings.TrimSpace(item)))
		if _, ok := validScopes[scope]; !ok {
			return nil, "must be one or more of revision, activation, allocation"
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, scope)
	}
	sortScopes(scopes)
	return scopes, ""
}

func sortScopes(scopes []domain.FreezeScope) {
	order := map[domain.FreezeScope]int{
		domain.FreezeScopeRevision:   1,
		domain.FreezeScopeActivation: 2,
		domain.FreezeScopeAllocation: 3,
	}
	for i := 1; i < len(scopes); i++ {
		for j := i; j > 0 && order[scopes[j-1]] > order[scopes[j]]; j-- {
			scopes[j-1], scopes[j] = scopes[j], scopes[j-1]
		}
	}
}

func parseBoundary(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func canonicalName(value string) string {
	return strings.TrimSuffix(strings.TrimSpace(strings.ToLower(value)), ".")
}

func validateZone(value string) error {
	if len(value) > domain.MaxZoneRunes {
		return domain.Validation("is too long", nil)
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 {
			return domain.Validation("contains an invalid label length", nil)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return domain.Validation("labels cannot start or end with a hyphen", nil)
		}
		for _, r := range label {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
				continue
			}
			return domain.Validation("contains unsupported characters", nil)
		}
	}
	return nil
}
