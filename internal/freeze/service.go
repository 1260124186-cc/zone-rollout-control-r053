package freeze

import (
	"context"
	"sort"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
	"solo-0014-zone-rollout-control/internal/store"
)

type Service struct {
	store *store.Store
	clock platform.Clock
	ids   platform.IDGenerator
}

func New(stateStore *store.Store, clock platform.Clock, ids platform.IDGenerator) *Service {
	return &Service{store: stateStore, clock: clock, ids: ids}
}

func (s *Service) Register(ctx context.Context, input domain.FreezeCreateInput) (domain.ChangeFreeze, error) {
	prepared, err := PrepareCreate(input, s.clock.Now())
	if err != nil {
		return domain.ChangeFreeze{}, err
	}
	now := s.clock.Now().UTC()
	created := domain.ChangeFreeze{
		ID:           s.ids.New("frz"),
		Zone:         prepared.Zone,
		Scopes:       prepared.Scopes,
		StartsAt:     prepared.StartsAt,
		EndsAt:       prepared.EndsAt,
		Reason:       prepared.Reason,
		AllowedRoles: append([]string(nil), prepared.AllowedRoles...),
		Status:       StatusAt(domain.ChangeFreeze{StartsAt: prepared.StartsAt, EndsAt: prepared.EndsAt}, now),
		Version:      1,
		CreatedBy:    prepared.CreatedBy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err = s.store.Write(ctx, func(state *domain.State) error {
		state.Freezes[created.ID] = created
		store.AddAudit(state, s.ids, "freeze", created.ID, "registered", now, map[string]any{
			"zone":   created.Zone,
			"scopes": created.Scopes,
		})
		return nil
	})
	return created, err
}

func (s *Service) Get(ctx context.Context, freezeID string) (domain.ChangeFreeze, error) {
	var found domain.ChangeFreeze
	err := s.store.Read(ctx, func(state *domain.State) error {
		item, ok := state.Freezes[freezeID]
		if !ok {
			return domain.NotFound("freeze", freezeID)
		}
		found = decorate(item, s.clock.Now())
		return nil
	})
	return found, err
}

func (s *Service) List(
	ctx context.Context,
	statusFilter domain.FreezeStatus,
	zoneFilter string,
	limit int,
	cursor string,
) (domain.FreezePage, error) {
	if limit < 1 || limit > 100 {
		return domain.FreezePage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	if statusFilter != "" && !validStatus(statusFilter) {
		return domain.FreezePage{}, domain.Validation("freeze status is not supported", map[string]any{
			"status": string(statusFilter),
		})
	}
	zoneFilter = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(zoneFilter)), ".")
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.FreezePage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.FreezePage
	now := s.clock.Now()
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.ChangeFreeze, 0, len(state.Freezes))
		for _, item := range state.Freezes {
			item = decorate(item, now)
			if statusFilter != "" && item.Status != statusFilter {
				continue
			}
			if zoneFilter != "" && item.Zone != zoneFilter {
				continue
			}
			if after != "" && item.ID <= after {
				continue
			}
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

func (s *Service) Update(ctx context.Context, freezeID string, input domain.FreezeUpdateInput) (domain.ChangeFreeze, error) {
	if input.ExpectedVersion == nil || *input.ExpectedVersion == 0 {
		return domain.ChangeFreeze{}, domain.Validation("expected_version must be greater than zero", nil)
	}
	prepared, err := PrepareUpdate(input)
	if err != nil {
		return domain.ChangeFreeze{}, err
	}
	var updated domain.ChangeFreeze
	err = s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Freezes[freezeID]
		if !ok {
			return domain.NotFound("freeze", freezeID)
		}
		if current.Version != *input.ExpectedVersion {
			return versionConflict(freezeID, *input.ExpectedVersion, current.Version)
		}
		if current.RevokedAt != nil {
			return domain.InvalidState("revoked freeze cannot be updated", map[string]any{
				"freeze_id": freezeID,
			})
		}
		if prepared.Scopes != nil {
			current.Scopes = *prepared.Scopes
		}
		if prepared.StartsAt != nil {
			current.StartsAt = *prepared.StartsAt
		}
		if prepared.EndsAt != nil {
			if prepared.EndsAt.IsZero() {
				current.EndsAt = nil
			} else {
				current.EndsAt = prepared.EndsAt
			}
		}
		if prepared.Reason != nil {
			current.Reason = *prepared.Reason
		}
		if prepared.AllowedRoles != nil {
			current.AllowedRoles = append([]string(nil), (*prepared.AllowedRoles)...)
		}
		if err := ValidateSchedule(current.StartsAt, current.EndsAt); err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		current.Version++
		current.UpdatedAt = now
		current.Status = StatusAt(current, now)
		state.Freezes[freezeID] = current
		store.AddAudit(state, s.ids, "freeze", freezeID, "updated", now, map[string]any{
			"version": current.Version,
		})
		updated = current
		return nil
	})
	return updated, err
}

// Revoke ends a freeze immediately. Revocation is prospective only: the
// freeze stops matching at the revocation instant, but FreezeBlock
// records produced while it was active are immutable and stay queryable.
func (s *Service) Revoke(ctx context.Context, freezeID string, input domain.FreezeRevokeInput) (domain.ChangeFreeze, error) {
	if input.ExpectedVersion == 0 {
		return domain.ChangeFreeze{}, domain.Validation("expected_version must be greater than zero", nil)
	}
	by, note, err := PrepareRevoke(input)
	if err != nil {
		return domain.ChangeFreeze{}, err
	}
	var updated domain.ChangeFreeze
	err = s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.Freezes[freezeID]
		if !ok {
			return domain.NotFound("freeze", freezeID)
		}
		if current.Version != input.ExpectedVersion {
			return versionConflict(freezeID, input.ExpectedVersion, current.Version)
		}
		if current.RevokedAt != nil {
			return domain.InvalidState("freeze is already revoked", map[string]any{
				"freeze_id": freezeID,
			})
		}
		now := s.clock.Now().UTC()
		current.RevokedAt = &now
		current.RevokedBy = by
		current.RevocationNote = note
		current.Version++
		current.UpdatedAt = now
		current.Status = domain.FreezeStatusRevoked
		state.Freezes[freezeID] = current
		store.AddAudit(state, s.ids, "freeze", freezeID, "revoked", now, map[string]any{
			"version": current.Version,
			"by":      by,
		})
		updated = current
		return nil
	})
	return updated, err
}

func (s *Service) ListBlocks(
	ctx context.Context,
	freezeID string,
	planID string,
	scope domain.FreezeScope,
	limit int,
	cursor string,
) (domain.FreezeBlockPage, error) {
	if limit < 1 || limit > 100 {
		return domain.FreezeBlockPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	if scope != "" {
		if _, ok := map[domain.FreezeScope]struct{}{
			domain.FreezeScopeRevision:   {},
			domain.FreezeScopeActivation: {},
			domain.FreezeScopeAllocation: {},
		}[scope]; !ok {
			return domain.FreezeBlockPage{}, domain.Validation("freeze scope is not supported", map[string]any{
				"scope": string(scope),
			})
		}
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.FreezeBlockPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.FreezeBlockPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.FreezeBlock, 0, len(state.FreezeBlocks))
		for _, item := range state.FreezeBlocks {
			if freezeID != "" && item.FreezeID != freezeID {
				continue
			}
			if planID != "" && item.PlanID != planID {
				continue
			}
			if scope != "" && item.Scope != scope {
				continue
			}
			if after != "" && item.ID <= after {
				continue
			}
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = limit
		if len(items) > limit {
			page.Items = append(page.Items, items[:limit]...)
			page.Meta.NextCursor = platform.EncodeCursor(items[limit-1].ID)
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

// Guard performs freeze enforcement inside another service's write
// transaction, so the interception record and any state change share one
// atomic snapshot.
type Guard struct {
	ids   platform.IDGenerator
	clock platform.Clock
}

func NewGuard(ids platform.IDGenerator, clock platform.Clock) *Guard {
	return &Guard{ids: ids, clock: clock}
}

// Check evaluates active freezes for the entry. When blocked it appends
// the immutable block record plus an audit event to state and returns the
// shared explanation. The caller must stop mutating and commit the
// transaction (return nil), then surface the returned error to the
// client; this persists the interception even though the entry itself is
// rejected.
func (g *Guard) Check(
	state *domain.State,
	zone string,
	scope domain.FreezeScope,
	planID string,
	actorRoles []string,
	startedBefore func(startsAt time.Time) bool,
) *domain.DomainError {
	now := g.clock.Now().UTC()
	freezes := make([]domain.ChangeFreeze, 0, len(state.Freezes))
	for _, item := range state.Freezes {
		freezes = append(freezes, item)
	}
	ev := Evaluate(freezes, zone, scope, actorRoles, startedBefore, now)
	if !ev.Blocked {
		return nil
	}
	block := domain.FreezeBlock{
		ID:         g.ids.New("blk"),
		FreezeID:   ev.Primary.ID,
		Zone:       zone,
		Scope:      scope,
		PlanID:     planID,
		ActorRole:  primaryRole(actorRoles),
		OccurredAt: now,
		Reason:     ev.Primary.Reason,
	}
	state.FreezeBlocks[block.ID] = block
	store.AddAudit(state, g.ids, "freeze_block", block.ID, "blocked", now, map[string]any{
		"freeze_id": ev.Primary.ID,
		"scope":     string(scope),
		"zone":      zone,
		"plan_id":   planID,
	})
	return FrozenError(ev, zone, scope, planID, actorRoles)
}

func primaryRole(roles []string) string {
	normalized := normalizeRoles(roles)
	if len(normalized) == 0 {
		return ""
	}
	return normalized[0]
}

func decorate(item domain.ChangeFreeze, now time.Time) domain.ChangeFreeze {
	item.Status = StatusAt(item, now)
	if item.Scopes == nil {
		item.Scopes = []domain.FreezeScope{}
	}
	if item.AllowedRoles == nil {
		item.AllowedRoles = []string{}
	}
	return item
}

func validStatus(status domain.FreezeStatus) bool {
	switch status {
	case domain.FreezeStatusScheduled, domain.FreezeStatusActive, domain.FreezeStatusExpired, domain.FreezeStatusRevoked:
		return true
	default:
		return false
	}
}

func versionConflict(id string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "freeze version does not match", map[string]any{
		"freeze_id": id,
		"expected":  expected,
		"actual":    actual,
	})
}
