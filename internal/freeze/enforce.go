package freeze

import (
	"sort"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// evaluationBoundary defines when a freeze covers an entry. A freeze is
// effective on the half-open interval [StartsAt, EndsAt): the start
// instant itself is frozen, and the end instant releases it. Comparing
// absolute UTC instants makes the boundary independent of the time zone
// offsets used when registering the freeze.
func activeAt(freeze domain.ChangeFreeze, now time.Time) bool {
	now = now.UTC()
	if freeze.RevokedAt != nil {
		return false
	}
	if now.Before(freeze.StartsAt.UTC()) {
		return false
	}
	if freeze.EndsAt != nil && !now.Before(freeze.EndsAt.UTC()) {
		return false
	}
	return true
}

// StatusAt derives the externally visible status of a freeze at an
// instant. Revocation is terminal and wins even when the schedule would
// otherwise be active.
func StatusAt(freeze domain.ChangeFreeze, now time.Time) domain.FreezeStatus {
	now = now.UTC()
	if freeze.RevokedAt != nil {
		return domain.FreezeStatusRevoked
	}
	if now.Before(freeze.StartsAt.UTC()) {
		return domain.FreezeStatusScheduled
	}
	if freeze.EndsAt != nil && !now.Before(freeze.EndsAt.UTC()) {
		return domain.FreezeStatusExpired
	}
	return domain.FreezeStatusActive
}

// Evaluation is the deterministic outcome of a freeze check.
type Evaluation struct {
	Blocked     bool
	Primary     domain.ChangeFreeze
	MatchingIDs []string
}

// Evaluate decides whether an entry is blocked by the given freeze set.
// Every active freeze whose zone and scope match and whose role list does
// not contain the actor contributes to the outcome. startedBefore reports
// whether the requesting workflow legally began before that freeze took
// effect; such in-flight workflows are allowed to continue for that
// freeze. Overlapping freezes therefore combine conjunctively: the entry
// proceeds only when every matching freeze exempts it.
func Evaluate(
	freezes []domain.ChangeFreeze,
	zone string,
	scope domain.FreezeScope,
	actorRoles []string,
	startedBefore func(startsAt time.Time) bool,
	now time.Time,
) Evaluation {
	roles := normalizeRoles(actorRoles)
	var blocking []domain.ChangeFreeze
	for _, candidate := range freezes {
		if !activeAt(candidate, now) {
			continue
		}
		if !coversZone(candidate, zone) || !coversScope(candidate, scope) {
			continue
		}
		if hasRole(candidate.AllowedRoles, roles) {
			continue
		}
		if startedBefore != nil && startedBefore(candidate.StartsAt.UTC()) {
			continue
		}
		blocking = append(blocking, candidate)
	}
	if len(blocking) == 0 {
		return Evaluation{Blocked: false, MatchingIDs: []string{}}
	}
	sort.Slice(blocking, func(i, j int) bool {
		if !blocking[i].StartsAt.Equal(blocking[j].StartsAt) {
			return blocking[i].StartsAt.Before(blocking[j].StartsAt)
		}
		return blocking[i].ID < blocking[j].ID
	})
	ids := make([]string, 0, len(blocking))
	for _, item := range blocking {
		ids = append(ids, item.ID)
	}
	return Evaluation{Blocked: true, Primary: blocking[0], MatchingIDs: ids}
}

func coversZone(freeze domain.ChangeFreeze, zone string) bool {
	return freeze.Zone == "" || freeze.Zone == zone
}

func coversScope(freeze domain.ChangeFreeze, scope domain.FreezeScope) bool {
	for _, candidate := range freeze.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

func hasRole(allowed []string, actor []string) bool {
	if len(actor) == 0 {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, role := range allowed {
		allowedSet[role] = struct{}{}
	}
	for _, role := range actor {
		if _, ok := allowedSet[role]; ok {
			return true
		}
	}
	return false
}

func normalizeRoles(roles []string) []string {
	seen := make(map[string]struct{}, len(roles))
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" {
			continue
		}
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		normalized = append(normalized, role)
	}
	sort.Strings(normalized)
	return normalized
}

// FrozenError builds the identical explanation returned by the revision,
// activation and allocation entries when a freeze blocks them.
func FrozenError(
	ev Evaluation,
	zone string,
	scope domain.FreezeScope,
	planID string,
	actorRoles []string,
) *domain.DomainError {
	primary := ev.Primary
	details := map[string]any{
		"scope":               string(scope),
		"zone":                zone,
		"plan_id":             planID,
		"freeze_id":           primary.ID,
		"freeze_zone":         primary.Zone,
		"freeze_reason":       primary.Reason,
		"freeze_starts_at":    primary.StartsAt.UTC().Format(time.RFC3339),
		"matching_freeze_ids": ev.MatchingIDs,
		"actor_roles":         normalizeRoles(actorRoles),
	}
	if primary.EndsAt != nil {
		details["freeze_ends_at"] = primary.EndsAt.UTC().Format(time.RFC3339)
	}
	if len(primary.AllowedRoles) > 0 {
		details["allowed_roles"] = append([]string(nil), primary.AllowedRoles...)
	}
	return domain.NewError(domain.CodeChangeFrozen, "change is blocked by an active change freeze", details)
}
