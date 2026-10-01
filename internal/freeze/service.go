package freeze

import (
	"context"
	"sort"
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

func (s *Service) Create(ctx context.Context, input domain.FreezePolicyInput) (domain.FreezePolicy, error) {
	now := s.clock.Now().UTC()
	if err := domain.ValidateFreezePolicyInput(input, now); err != nil {
		return domain.FreezePolicy{}, err
	}
	input = domain.NormalizeFreezePolicyInput(input)
	policy := domain.FreezePolicy{
		ID:             s.ids.New("frz"),
		Zone:           input.Zone,
		Scope:          domain.FreezeScope(input.Scope),
		Reason:         input.Reason,
		ExceptionRoles: input.ExceptionRoles,
		StartsAt:       input.StartsAt,
		EndsAt:         input.EndsAt,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		state.FreezePolicies[policy.ID] = policy
		store.AddAudit(state, s.ids, "freeze_policy", policy.ID, "created", now, map[string]any{
			"zone":  policy.Zone,
			"scope": policy.Scope,
		})
		return nil
	})
	return policy, err
}

func (s *Service) Update(
	ctx context.Context,
	policyID string,
	input domain.FreezePolicyUpdateInput,
) (domain.FreezePolicy, error) {
	now := s.clock.Now().UTC()
	if err := domain.ValidateFreezePolicyUpdateInput(input, now); err != nil {
		return domain.FreezePolicy{}, err
	}
	input = domain.NormalizeFreezePolicyUpdateInput(input)
	var updated domain.FreezePolicy
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.FreezePolicies[policyID]
		if !ok {
			return domain.NotFound("freeze_policy", policyID)
		}
		if current.Version != input.ExpectedVersion {
			return versionConflict(policyID, input.ExpectedVersion, current.Version)
		}
		if current.RevokedAt != nil {
			return domain.InvalidState("a revoked freeze policy cannot be updated", map[string]any{
				"policy_id": policyID,
			})
		}
		current.Scope = domain.FreezeScope(input.Scope)
		current.Reason = input.Reason
		current.ExceptionRoles = input.ExceptionRoles
		current.StartsAt = input.StartsAt
		current.EndsAt = input.EndsAt
		current.UpdatedAt = now
		current.Version++
		state.FreezePolicies[policyID] = current
		store.AddAudit(state, s.ids, "freeze_policy", policyID, "updated", now, map[string]any{
			"version": current.Version,
		})
		updated = current
		return nil
	})
	return updated, err
}

func (s *Service) Revoke(
	ctx context.Context,
	policyID string,
	input domain.FreezeRevokeInput,
) (domain.FreezePolicy, error) {
	if input.ExpectedVersion == 0 {
		return domain.FreezePolicy{}, domain.Validation("expected_version must be greater than zero", nil)
	}
	now := s.clock.Now().UTC()
	var revoked domain.FreezePolicy
	err := s.store.Write(ctx, func(state *domain.State) error {
		current, ok := state.FreezePolicies[policyID]
		if !ok {
			return domain.NotFound("freeze_policy", policyID)
		}
		if current.Version != input.ExpectedVersion {
			return versionConflict(policyID, input.ExpectedVersion, current.Version)
		}
		if current.RevokedAt != nil {
			return domain.InvalidState("freeze policy is already revoked", map[string]any{
				"policy_id": policyID,
			})
		}
		current.RevokedAt = timePointer(now)
		current.UpdatedAt = now
		current.Version++
		state.FreezePolicies[policyID] = current
		// Interceptions recorded while the policy was in force stay untouched:
		// revocation stops future blocks, it never rewrites the past.
		store.AddAudit(state, s.ids, "freeze_policy", policyID, "revoked", now, map[string]any{
			"version": current.Version,
		})
		revoked = current
		return nil
	})
	return revoked, err
}

func (s *Service) Get(ctx context.Context, policyID string) (domain.FreezePolicy, error) {
	var policy domain.FreezePolicy
	err := s.store.Read(ctx, func(state *domain.State) error {
		found, ok := state.FreezePolicies[policyID]
		if !ok {
			return domain.NotFound("freeze_policy", policyID)
		}
		policy = found
		return nil
	})
	return policy, err
}

func (s *Service) List(
	ctx context.Context,
	zone string,
	activeOnly bool,
	limit int,
	cursor string,
) (domain.FreezePolicyPage, error) {
	if limit < 1 || limit > 100 {
		return domain.FreezePolicyPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.FreezePolicyPage{}, domain.Validation("cursor is invalid", nil)
	}
	now := s.clock.Now().UTC()
	var page domain.FreezePolicyPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.FreezePolicy, 0, len(state.FreezePolicies))
		for _, policy := range state.FreezePolicies {
			if zone != "" && policy.Zone != zone {
				continue
			}
			if activeOnly && !policy.Active(now) {
				continue
			}
			if after != "" && policy.ID <= after {
				continue
			}
			items = append(items, policy)
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

func (s *Service) ListInterceptions(
	ctx context.Context,
	zone string,
	policyID string,
	limit int,
	cursor string,
) (domain.FreezeInterceptionPage, error) {
	if limit < 1 || limit > 100 {
		return domain.FreezeInterceptionPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": limit,
		})
	}
	after, err := platform.DecodeCursor(cursor)
	if err != nil {
		return domain.FreezeInterceptionPage{}, domain.Validation("cursor is invalid", nil)
	}
	var page domain.FreezeInterceptionPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.FreezeInterception, 0, len(state.FreezeInterceptions))
		for _, interception := range state.FreezeInterceptions {
			if zone != "" && interception.Zone != zone {
				continue
			}
			if policyID != "" && interception.PolicyID != policyID {
				continue
			}
			if after != "" && interception.ID <= after {
				continue
			}
			items = append(items, interception)
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

// RecordInterception persists one blocked attempt in its own transaction. It
// runs after the guarded mutation was rejected, so the record is committed
// even though the blocked change itself left no trace. The zone is the
// attempted plan zone, which may differ from a wildcard policy zone.
func (s *Service) RecordInterception(
	ctx context.Context,
	policy domain.FreezePolicy,
	operation domain.FreezeOperation,
	planID string,
	targetZone string,
	actorRole string,
) (domain.FreezeInterception, error) {
	now := s.clock.Now().UTC()
	interception := domain.FreezeInterception{
		ID:        s.ids.New("fri"),
		PolicyID:  policy.ID,
		Zone:      targetZone,
		Operation: operation,
		PlanID:    planID,
		ActorRole: actorRole,
		Reason:    policy.Reason,
		Message:   domain.FreezeBlockMessage(policy),
		At:        now,
	}
	err := s.store.Write(ctx, func(state *domain.State) error {
		state.FreezeInterceptions[interception.ID] = interception
		store.AddAudit(state, s.ids, "freeze_interception", interception.ID, "recorded", now, map[string]any{
			"policy_id": policy.ID,
			"operation": operation,
			"plan_id":   planID,
		})
		return nil
	})
	return interception, err
}

func versionConflict(id string, expected uint64, actual uint64) *domain.DomainError {
	return domain.NewError(domain.CodeVersionConflict, "freeze policy version does not match", map[string]any{
		"policy_id": id,
		"expected":  expected,
		"actual":    actual,
	})
}

func timePointer(value time.Time) *time.Time {
	return &value
}
