package domain

import "time"

type FreezeScope string

const (
	FreezeScopeRevision   FreezeScope = "revision"
	FreezeScopeActivation FreezeScope = "activation"
	FreezeScopeAllocation FreezeScope = "allocation"
)

type FreezeStatus string

const (
	FreezeStatusScheduled FreezeStatus = "scheduled"
	FreezeStatusActive    FreezeStatus = "active"
	FreezeStatusExpired   FreezeStatus = "expired"
	FreezeStatusRevoked   FreezeStatus = "revoked"
)

// ChangeFreeze registers a change freeze for a zone (or every zone when
// Zone is empty). Scopes lists the entries that must reject changes while
// the freeze is effective; AllowedRoles names operator roles exempt from
// the freeze.
type ChangeFreeze struct {
	ID             string        `json:"id"`
	Zone           string        `json:"zone,omitempty"`
	Scopes         []FreezeScope `json:"scopes"`
	StartsAt       time.Time     `json:"starts_at"`
	EndsAt         *time.Time    `json:"ends_at,omitempty"`
	Reason         string        `json:"reason"`
	AllowedRoles   []string      `json:"allowed_roles"`
	Status         FreezeStatus  `json:"status"`
	Version        uint64        `json:"version"`
	CreatedBy      string        `json:"created_by,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	RevokedAt      *time.Time    `json:"revoked_at,omitempty"`
	RevokedBy      string        `json:"revoked_by,omitempty"`
	RevocationNote string        `json:"revocation_note,omitempty"`
}

// FreezeBlock is an immutable record that a change freeze rejected an
// entry. Records survive freeze expiry and revocation so undoing a policy
// can never retroactively erase the interceptions it caused.
type FreezeBlock struct {
	ID         string      `json:"id"`
	FreezeID   string      `json:"freeze_id"`
	Zone       string      `json:"zone"`
	Scope      FreezeScope `json:"scope"`
	PlanID     string      `json:"plan_id"`
	ActorRole  string      `json:"actor_role,omitempty"`
	OccurredAt time.Time   `json:"occurred_at"`
	Reason     string      `json:"reason"`
}

// FreezeCreateInput registers a new change freeze.
type FreezeCreateInput struct {
	Zone         string   `json:"zone"`
	Scopes       []string `json:"scopes"`
	StartsAt     string   `json:"starts_at"`
	EndsAt       string   `json:"ends_at,omitempty"`
	Reason       string   `json:"reason"`
	AllowedRoles []string `json:"allowed_roles"`
	CreatedBy    string   `json:"created_by"`
}

// FreezeUpdateInput modifies a registered freeze. Pointer fields are
// updated only when present in the request body; an explicit null clears
// the corresponding value.
type FreezeUpdateInput struct {
	ExpectedVersion *uint64   `json:"expected_version"`
	Scopes          *[]string `json:"scopes"`
	StartsAt        *string   `json:"starts_at"`
	EndsAt          *string   `json:"ends_at"`
	ClearEndsAt     bool      `json:"-"`
	Reason          *string   `json:"reason"`
	AllowedRoles    *[]string `json:"allowed_roles"`
	UpdatedBy       string    `json:"updated_by"`
}

// FreezeRevokeInput revokes a registered freeze. Revocation is not
// retroactive: already recorded blocks remain in place.
type FreezeRevokeInput struct {
	ExpectedVersion uint64 `json:"expected_version"`
	RevokedBy       string `json:"revoked_by"`
	Note            string `json:"note"`
}

type FreezePage struct {
	Items []ChangeFreeze `json:"items"`
	Meta  PageMeta       `json:"meta"`
}

type FreezeBlockPage struct {
	Items []FreezeBlock `json:"items"`
	Meta  PageMeta      `json:"meta"`
}
