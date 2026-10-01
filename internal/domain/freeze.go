package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	FreezeWildcardZone   = "*"
	MinFreezeReasonRunes = 4
	MaxFreezeReasonRunes = 240
	MaxFreezeRoles       = 8
	MinFreezeRoleRunes   = 2
	MaxFreezeRoleRunes   = 64
)

// FreezeScope selects which change entry points a policy covers.
type FreezeScope string

const (
	FreezeScopeAll        FreezeScope = "all"
	FreezeScopeRevision   FreezeScope = "revision"
	FreezeScopeActivation FreezeScope = "activation"
	FreezeScopeAllocation FreezeScope = "allocation"
)

// FreezeOperation identifies one guarded change entry point.
type FreezeOperation string

const (
	FreezeOpRevisionAppend FreezeOperation = "revision_append"
	FreezeOpActivation     FreezeOperation = "activation"
	FreezeOpAllocation     FreezeOperation = "allocation"
)

// FreezePolicy blocks zone changes inside a half-open UTC window
// [starts_at, ends_at). A policy lapses when now reaches ends_at or when it
// is revoked; revocation never removes interceptions already recorded.
type FreezePolicy struct {
	ID             string      `json:"id"`
	Zone           string      `json:"zone"`
	Scope          FreezeScope `json:"scope"`
	Reason         string      `json:"reason"`
	ExceptionRoles []string    `json:"exception_roles"`
	StartsAt       time.Time   `json:"starts_at"`
	EndsAt         time.Time   `json:"ends_at"`
	Version        uint64      `json:"version"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
	RevokedAt      *time.Time  `json:"revoked_at,omitempty"`
}

// FreezeInterception is an immutable record of one blocked change attempt.
// Records survive policy updates, expiry, and revocation.
type FreezeInterception struct {
	ID        string          `json:"id"`
	PolicyID  string          `json:"policy_id"`
	Zone      string          `json:"zone"`
	Operation FreezeOperation `json:"operation"`
	PlanID    string          `json:"plan_id"`
	ActorRole string          `json:"actor_role,omitempty"`
	Reason    string          `json:"reason"`
	Message   string          `json:"message"`
	At        time.Time       `json:"at"`
}

type FreezePolicyInput struct {
	Zone           string    `json:"zone"`
	Scope          string    `json:"scope"`
	Reason         string    `json:"reason"`
	ExceptionRoles []string  `json:"exception_roles"`
	StartsAt       time.Time `json:"starts_at"`
	EndsAt         time.Time `json:"ends_at"`
}

type FreezePolicyUpdateInput struct {
	ExpectedVersion uint64    `json:"expected_version"`
	Scope           string    `json:"scope"`
	Reason          string    `json:"reason"`
	ExceptionRoles  []string  `json:"exception_roles"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
}

type FreezeRevokeInput struct {
	ExpectedVersion uint64 `json:"expected_version"`
}

func NormalizeFreezePolicyInput(input FreezePolicyInput) FreezePolicyInput {
	input.Zone = normalizeFreezeZone(input.Zone)
	input.Scope = strings.TrimSpace(strings.ToLower(input.Scope))
	input.Reason = strings.TrimSpace(input.Reason)
	input.ExceptionRoles = normalizeFreezeRoles(input.ExceptionRoles)
	input.StartsAt = input.StartsAt.UTC()
	input.EndsAt = input.EndsAt.UTC()
	return input
}

func ValidateFreezePolicyInput(input FreezePolicyInput, now time.Time) error {
	input = NormalizeFreezePolicyInput(input)
	details := make(map[string]any)
	if input.Zone != FreezeWildcardZone {
		if err := validateCanonicalName(input.Zone); err != nil {
			details["zone"] = err.Error()
		}
	}
	if err := validateFreezeWindow(input.StartsAt, input.EndsAt, now); err != nil {
		details["window"] = err.Error()
	}
	collectFreezeFieldErrors(details, input.Scope, input.Reason, input.ExceptionRoles)
	if len(details) > 0 {
		return Validation("freeze policy input is invalid", details)
	}
	return nil
}

func NormalizeFreezePolicyUpdateInput(input FreezePolicyUpdateInput) FreezePolicyUpdateInput {
	input.Scope = strings.TrimSpace(strings.ToLower(input.Scope))
	input.Reason = strings.TrimSpace(input.Reason)
	input.ExceptionRoles = normalizeFreezeRoles(input.ExceptionRoles)
	input.StartsAt = input.StartsAt.UTC()
	input.EndsAt = input.EndsAt.UTC()
	return input
}

func ValidateFreezePolicyUpdateInput(input FreezePolicyUpdateInput, now time.Time) error {
	input = NormalizeFreezePolicyUpdateInput(input)
	details := make(map[string]any)
	if input.ExpectedVersion == 0 {
		details["expected_version"] = "must be greater than zero"
	}
	if err := validateFreezeWindow(input.StartsAt, input.EndsAt, now); err != nil {
		details["window"] = err.Error()
	}
	collectFreezeFieldErrors(details, input.Scope, input.Reason, input.ExceptionRoles)
	if len(details) > 0 {
		return Validation("freeze policy update is invalid", details)
	}
	return nil
}

func collectFreezeFieldErrors(details map[string]any, scope string, reason string, roles []string) {
	switch FreezeScope(scope) {
	case FreezeScopeAll, FreezeScopeRevision, FreezeScopeActivation, FreezeScopeAllocation:
	default:
		details["scope"] = "must be one of all, revision, activation, allocation"
	}
	if count := utf8.RuneCountInString(reason); count < MinFreezeReasonRunes || count > MaxFreezeReasonRunes {
		details["reason"] = "must contain between 4 and 240 characters"
	}
	if len(roles) > MaxFreezeRoles {
		details["exception_roles"] = "must contain at most 8 roles"
		return
	}
	for _, role := range roles {
		if err := validateFreezeRole(role); err != nil {
			details["exception_roles"] = err.Error()
			return
		}
	}
}

func validateFreezeWindow(startsAt time.Time, endsAt time.Time, now time.Time) error {
	if startsAt.IsZero() || endsAt.IsZero() {
		return Validation("starts_at and ends_at are required", nil)
	}
	if !endsAt.After(startsAt) {
		return Validation("ends_at must be after starts_at", nil)
	}
	if !endsAt.After(now.UTC()) {
		return Validation("ends_at must be in the future", nil)
	}
	return nil
}

func validateFreezeRole(role string) error {
	count := utf8.RuneCountInString(role)
	if count < MinFreezeRoleRunes || count > MaxFreezeRoleRunes {
		return Validation("roles must contain between 2 and 64 characters", nil)
	}
	for _, r := range role {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return Validation("roles may contain only lowercase letters, digits, hyphens, and underscores", nil)
	}
	return nil
}

func normalizeFreezeZone(zone string) string {
	trimmed := strings.TrimSpace(zone)
	if trimmed == FreezeWildcardZone {
		return FreezeWildcardZone
	}
	return canonicalName(trimmed)
}

func normalizeFreezeRoles(roles []string) []string {
	seen := make(map[string]struct{}, len(roles))
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(strings.ToLower(role))
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

// NormalizeActorRole canonicalizes the caller role supplied by the transport
// layer so it can be compared with stored exception roles.
func NormalizeActorRole(role string) string {
	return strings.TrimSpace(strings.ToLower(role))
}

// Active reports whether the policy is in force at the given instant. The
// window is half-open: the freeze starts exactly at starts_at and lapses
// exactly at ends_at. All instants are compared in UTC, so clients may write
// the window with any UTC offset.
func (p FreezePolicy) Active(now time.Time) bool {
	if p.RevokedAt != nil {
		return false
	}
	now = now.UTC()
	return !now.Before(p.StartsAt.UTC()) && now.Before(p.EndsAt.UTC())
}

func (p FreezePolicy) MatchesZone(zone string) bool {
	return p.Zone == FreezeWildcardZone || p.Zone == zone
}

func (p FreezePolicy) Covers(operation FreezeOperation) bool {
	switch p.Scope {
	case FreezeScopeAll:
		return true
	case FreezeScopeRevision:
		return operation == FreezeOpRevisionAppend
	case FreezeScopeActivation:
		return operation == FreezeOpActivation
	case FreezeScopeAllocation:
		return operation == FreezeOpAllocation
	default:
		return false
	}
}

func (p FreezePolicy) AllowsRole(role string) bool {
	if role == "" {
		return false
	}
	for _, allowed := range p.ExceptionRoles {
		if allowed == role {
			return true
		}
	}
	return false
}

// BlockingFreeze evaluates every policy against one change attempt. Overlaps
// resolve strictly: the attempt is blocked when at least one active matching
// policy does not list the actor role as an exception. The blocking policy is
// selected deterministically by (starts_at, id) so all guarded entry points
// explain the same policy for the same zone and instant.
func BlockingFreeze(
	state *State,
	zone string,
	operation FreezeOperation,
	actorRole string,
	now time.Time,
) (FreezePolicy, bool) {
	matches := make([]FreezePolicy, 0)
	for _, policy := range state.FreezePolicies {
		if !policy.Active(now) || !policy.MatchesZone(zone) || !policy.Covers(operation) {
			continue
		}
		if policy.AllowsRole(actorRole) {
			continue
		}
		matches = append(matches, policy)
	}
	if len(matches) == 0 {
		return FreezePolicy{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].StartsAt.Equal(matches[j].StartsAt) {
			return matches[i].StartsAt.Before(matches[j].StartsAt)
		}
		return matches[i].ID < matches[j].ID
	})
	return matches[0], true
}

// FreezeBlockMessage builds the single explanation returned by every guarded
// entry point. It depends only on the policy, never on the operation, so the
// revision, activation, and allocation doors all answer identically.
func FreezeBlockMessage(policy FreezePolicy) string {
	return fmt.Sprintf(
		"zone %s changes are frozen until %s: %s",
		policy.Zone,
		policy.EndsAt.UTC().Format(time.RFC3339),
		policy.Reason,
	)
}

// FreezeBlockedError builds the stable domain error for a blocked attempt.
// The message is identical at every guarded entry point; the attempted zone
// and operation travel in the details for operators and auditors.
func FreezeBlockedError(policy FreezePolicy, operation FreezeOperation, targetZone string) *DomainError {
	return NewError(CodeZoneFrozen, FreezeBlockMessage(policy), map[string]any{
		"policy_id":   policy.ID,
		"zone":        policy.Zone,
		"target_zone": targetZone,
		"scope":       policy.Scope,
		"operation":   operation,
		"reason":      policy.Reason,
		"starts_at":   policy.StartsAt.UTC().Format(time.RFC3339),
		"ends_at":     policy.EndsAt.UTC().Format(time.RFC3339),
	})
}
