package workflowprobe

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// freezeContext carries the plan and policy identifiers shared across the
// freeze scenario phases.
type freezeContext struct {
	planA   string
	planB   string
	planC   string
	leaseA1 string
	groupA1 string
	policy1 string
}

func runZoneFreezeEnforcement() error {
	h, err := newHarness()
	if err != nil {
		return err
	}
	defer h.close()

	ctx := &freezeContext{}
	phases := []func(*harness, *freezeContext) error{
		freezeSetup,
		freezeInputValidation,
		freezeBlocksAllDoors,
		freezeGrandfathering,
		freezeExceptionRole,
		freezeOverlapAndRevocation,
		freezeExpiry,
		freezeConcurrency,
		freezeWildcardScope,
	}
	for index, phase := range phases {
		if err := phase(h, ctx); err != nil {
			return fmt.Errorf("phase %d: %w", index+1, err)
		}
	}
	return nil
}

func freezeSetup(harness *harness, ctx *freezeContext) error {
	for _, name := range []string{"freeze-east-01", "freeze-east-02", "freeze-east-03"} {
		if _, err := enrollNode(harness, name, "east"); err != nil {
			return err
		}
	}
	planA, _, err := createActivePlan(harness, "edge.example", 2026092501, 1, 1, 80)
	if err != nil {
		return err
	}
	ctx.planA = planA.ID
	group, err := allocate(harness, planA.ID, "lease-req-a-001", "east", 1)
	if err != nil {
		return err
	}
	ctx.groupA1 = group.ID
	ctx.leaseA1 = group.LeaseIDs[0]

	var planB domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "edge.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &planB); err != nil {
		return err
	}
	ctx.planB = planB.ID
	if err := addRevision(harness, planB.ID, 2026092501, ""); err != nil {
		return err
	}

	planC, _, err := createActivePlan(harness, "other.example", 2026092501, 1, 1, 80)
	if err != nil {
		return err
	}
	ctx.planC = planC.ID
	return nil
}

func freezeInputValidation(harness *harness, _ *freezeContext) error {
	now := harness.clock.Now()
	base := domain.FreezePolicyInput{
		Zone:     "edge.example",
		Scope:    "all",
		Reason:   "valid reason",
		StartsAt: now.Add(-time.Hour),
		EndsAt:   now.Add(time.Hour),
	}
	cases := []domain.FreezePolicyInput{
		func() domain.FreezePolicyInput { in := base; in.StartsAt = now.Add(time.Hour); return in }(),
		func() domain.FreezePolicyInput { in := base; in.Scope = "banana"; return in }(),
		func() domain.FreezePolicyInput { in := base; in.Reason = "x"; return in }(),
		func() domain.FreezePolicyInput { in := base; in.Zone = "BAD ZONE!!"; return in }(),
		func() domain.FreezePolicyInput { in := base; in.EndsAt = now.Add(-time.Minute); return in }(),
		func() domain.FreezePolicyInput { in := base; in.ExceptionRoles = []string{"Bad Role"}; return in }(),
	}
	for index, input := range cases {
		err := harness.call(http.MethodPut, "/v1/freeze-policies", input, nil)
		if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
			return fmt.Errorf("validation case %d: %w", index, err)
		}
	}
	err := harness.call(http.MethodGet, "/v1/freeze-policies/frz_missing", nil, nil)
	return expectFailure(err, http.StatusNotFound, string(domain.CodeNotFound))
}

func freezeBlocksAllDoors(harness *harness, ctx *freezeContext) error {
	// The window is written with a +08:00 offset: 19:55+08:00 is 11:55 UTC,
	// so the freeze is already in force at the harness time 12:00 UTC.
	policy, err := createFreezePolicy(harness, domain.FreezePolicyInput{
		Zone:           "edge.example",
		Scope:          "all",
		Reason:         "National holiday event freeze",
		ExceptionRoles: []string{"Release_Manager"},
		StartsAt:       time.Date(2026, 9, 25, 19, 55, 0, 0, time.FixedZone("CST", 8*3600)),
		EndsAt:         time.Date(2026, 9, 25, 20, 30, 0, 0, time.FixedZone("CST", 8*3600)),
	})
	if err != nil {
		return err
	}
	ctx.policy1 = policy.ID
	if !policy.StartsAt.Equal(time.Date(2026, 9, 25, 11, 55, 0, 0, time.UTC)) {
		return ensure(false, "freeze window was not normalized across timezones")
	}
	if len(policy.ExceptionRoles) != 1 || policy.ExceptionRoles[0] != "release_manager" {
		return ensure(false, "exception roles were not normalized")
	}

	// All three guarded doors must fail with the identical explanation.
	err = addRevision(harness, ctx.planB, 2026092502, "")
	messageRevision, err := expectFreeze(err)
	if err != nil {
		return err
	}
	err = activatePlan(harness, ctx.planB, 2, "")
	messageActivation, err := expectFreeze(err)
	if err != nil {
		return err
	}
	err = allocateWithRole(harness, ctx.planA, "lease-req-a-002", "")
	messageAllocation, err := expectFreeze(err)
	if err != nil {
		return err
	}
	if messageRevision != messageActivation || messageRevision != messageAllocation {
		return ensure(false, "freeze explanation differs across entry points")
	}
	if !strings.Contains(messageRevision, "National holiday event freeze") ||
		!strings.Contains(messageRevision, "2026-09-25T12:30:00Z") ||
		!strings.Contains(messageRevision, "edge.example") {
		return ensure(false, "freeze explanation misses reason, expiry, or zone")
	}
	return nil
}

func freezeGrandfathering(harness *harness, ctx *freezeContext) error {
	// A lease claimed before the freeze still reports its probe.
	if _, err := submitProbe(harness, ctx.leaseA1, "probe-req-a-001", true, 2026092501, 12); err != nil {
		return fmt.Errorf("in-flight lease could not report under freeze: %w", err)
	}
	// A recorded allocation replays under the freeze.
	var replay domain.LeaseGroup
	if err := harness.call(http.MethodPatch, "/v1/plans/"+ctx.planA+"/leases", domain.LeaseInput{
		RequestKey: "lease-req-a-001", Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, &replay); err != nil {
		return fmt.Errorf("recorded allocation did not replay under freeze: %w", err)
	}
	if replay.ID != ctx.groupA1 {
		return ensure(false, "allocation replay returned a different group")
	}
	// An already active plan winds down normally.
	var completed domain.RolloutPlan
	if err := harness.call(http.MethodPatch, "/v1/plans/"+ctx.planA+"/complete", map[string]any{
		"expected_version": 3,
	}, &completed); err != nil {
		return fmt.Errorf("active plan could not complete under freeze: %w", err)
	}
	if completed.State != domain.PlanCompleted {
		return ensure(false, "active plan did not complete under freeze")
	}
	// A zone without any policy keeps the existing behavior.
	if _, err := allocate(harness, ctx.planC, "lease-req-c-001", "", 1); err != nil {
		return fmt.Errorf("unconfigured zone was affected by freeze: %w", err)
	}
	return nil
}

func freezeExceptionRole(harness *harness, ctx *freezeContext) error {
	activated, err := activatePlanOK(harness, ctx.planB, 2, "release_manager")
	if err != nil {
		return fmt.Errorf("exception role could not activate: %w", err)
	}
	if activated.State != domain.PlanActive {
		return ensure(false, "exception activation did not take effect")
	}
	err = allocateWithRole(harness, ctx.planB, "lease-req-b-001", "")
	if _, err := expectFreeze(err); err != nil {
		return err
	}
	// The blocked attempt left no idempotency record: the same request key
	// succeeds once the exception role is presented.
	if _, err := allocateOK(harness, ctx.planB, "lease-req-b-001", "release_manager"); err != nil {
		return fmt.Errorf("exception role could not allocate: %w", err)
	}
	return nil
}

func freezeOverlapAndRevocation(harness *harness, _ *freezeContext) error {
	// The overlapping policy starts earlier, so it wins the deterministic
	// selection even though it is registered second.
	policy2, err := createFreezePolicy(harness, domain.FreezePolicyInput{
		Zone:     "edge.example",
		Scope:    "all",
		Reason:   "Security incident freeze",
		StartsAt: time.Date(2026, 9, 25, 11, 50, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 9, 25, 12, 40, 0, 0, time.UTC),
	})
	if err != nil {
		return err
	}
	var planD domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "edge.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &planD); err != nil {
		return err
	}
	err = addRevision(harness, planD.ID, 2026092501, "")
	message, err := expectFreeze(err)
	if err != nil {
		return err
	}
	if !strings.Contains(message, "Security incident freeze") {
		return ensure(false, "overlapping freeze did not select the earliest window")
	}
	// The exception role from the first policy cannot bypass the second one.
	err = activatePlan(harness, planD.ID, 1, "release_manager")
	message, err = expectFreeze(err)
	if err != nil {
		return err
	}
	if !strings.Contains(message, "Security incident freeze") {
		return ensure(false, "exception role bypassed an overlapping freeze")
	}

	revoked, err := revokeFreezePolicy(harness, policy2.ID, 1)
	if err != nil {
		return err
	}
	if revoked.RevokedAt == nil {
		return ensure(false, "revocation did not set revoked_at")
	}
	err = revokeFreezePolicyErr(harness, policy2.ID, 2)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}
	err = harness.call(http.MethodPatch, "/v1/freeze-policies/"+policy2.ID, domain.FreezePolicyUpdateInput{
		ExpectedVersion: 2,
		Scope:           "all",
		Reason:          "Security incident freeze",
		StartsAt:        time.Date(2026, 9, 25, 11, 50, 0, 0, time.UTC),
		EndsAt:          time.Date(2026, 9, 25, 12, 40, 0, 0, time.UTC),
	}, nil)
	if err := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); err != nil {
		return err
	}

	// Revocation never erases recorded interceptions.
	var page domain.FreezeInterceptionPage
	if err := harness.call(http.MethodGet, "/v1/freeze-interceptions?zone=edge.example", nil, &page); err != nil {
		return err
	}
	if len(page.Items) != 6 {
		return fmt.Errorf("expected 6 recorded interceptions, got %d", len(page.Items))
	}
	fromRevoked := 0
	withRole := 0
	for _, item := range page.Items {
		if item.Message == "" || item.PolicyID == "" || item.Operation == "" {
			return ensure(false, "interception record is incomplete")
		}
		if item.Zone != "edge.example" {
			return ensure(false, "interception recorded the wrong target zone")
		}
		if item.PolicyID == policy2.ID {
			fromRevoked++
		}
		if item.ActorRole == "release_manager" {
			withRole++
		}
	}
	if fromRevoked != 2 {
		return ensure(false, "revocation erased recorded interceptions")
	}
	if withRole != 1 {
		return ensure(false, "interception did not keep the actor role")
	}

	// With the stricter policy revoked, the first policy's exception applies.
	if err := addRevision(harness, planD.ID, 2026092501, "release_manager"); err != nil {
		return fmt.Errorf("exception role could not revise after revocation: %w", err)
	}
	return nil
}

func freezeExpiry(harness *harness, _ *freezeContext) error {
	harness.clock.Advance(31 * time.Minute) // 12:31 UTC, past the 12:30 freeze end
	var planD domain.RolloutPlan
	// planD is the only remaining draft in edge.example; find it via the list.
	var plans domain.PlanPage
	if err := harness.call(http.MethodGet, "/v1/plans?state=draft", nil, &plans); err != nil {
		return err
	}
	for _, candidate := range plans.Items {
		if candidate.Zone == "edge.example" {
			planD = candidate
		}
	}
	if planD.ID == "" {
		return ensure(false, "could not locate the draft plan")
	}
	if err := addRevision(harness, planD.ID, 2026092502, ""); err != nil {
		return fmt.Errorf("lapsed freeze still blocks: %w", err)
	}
	var active domain.FreezePolicyPage
	if err := harness.call(http.MethodGet, "/v1/freeze-policies?active=true", nil, &active); err != nil {
		return err
	}
	if len(active.Items) != 0 {
		return ensure(false, "expired or revoked policies still count as active")
	}
	return nil
}

func freezeConcurrency(harness *harness, _ *freezeContext) error {
	var planE domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "sync.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &planE); err != nil {
		return err
	}
	if err := addRevision(harness, planE.ID, 2026092501, ""); err != nil {
		return err
	}
	now := harness.clock.Now()
	policy, err := createFreezePolicy(harness, domain.FreezePolicyInput{
		Zone:     "sync.example",
		Scope:    "all",
		Reason:   "Concurrency drill freeze",
		StartsAt: now.Add(-5 * time.Minute),
		EndsAt:   now.Add(30 * time.Minute),
	})
	if err != nil {
		return err
	}

	// Concurrent blocked activations all return the identical explanation.
	const attempts = 8
	messages := make([]string, attempts)
	var wait sync.WaitGroup
	for index := 0; index < attempts; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			err := activatePlan(harness, planE.ID, 2, "")
			message, freezeErr := expectFreeze(err)
			if freezeErr != nil {
				messages[slot] = "ERROR: " + freezeErr.Error()
				return
			}
			messages[slot] = message
		}(index)
	}
	wait.Wait()
	for _, message := range messages {
		if message != messages[0] || strings.HasPrefix(message, "ERROR: ") {
			return ensure(false, "concurrent freeze blocks diverged")
		}
	}

	// Concurrent updates with the same version produce exactly one winner.
	const updates = 6
	results := make([]error, updates)
	for index := 0; index < updates; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			results[slot] = harness.call(http.MethodPatch, "/v1/freeze-policies/"+policy.ID, domain.FreezePolicyUpdateInput{
				ExpectedVersion: 1,
				Scope:           "all",
				Reason:          "Concurrency drill freeze extended",
				StartsAt:        now.Add(-5 * time.Minute),
				EndsAt:          now.Add(30 * time.Minute),
			}, nil)
		}(index)
	}
	wait.Wait()
	wins := 0
	for _, result := range results {
		if result == nil {
			wins++
			continue
		}
		if err := expectFailure(result, http.StatusConflict, string(domain.CodeVersionConflict)); err != nil {
			return err
		}
	}
	if wins != 1 {
		return fmt.Errorf("expected exactly one concurrent update to win, got %d", wins)
	}

	var page domain.FreezeInterceptionPage
	if err := harness.call(http.MethodGet, "/v1/freeze-interceptions?zone=sync.example", nil, &page); err != nil {
		return err
	}
	if len(page.Items) != attempts {
		return fmt.Errorf("expected %d concurrent interceptions, got %d", attempts, len(page.Items))
	}
	return nil
}

func freezeWildcardScope(harness *harness, _ *freezeContext) error {
	var planF domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "other.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &planF); err != nil {
		return err
	}
	if err := addRevision(harness, planF.ID, 2026092501, ""); err != nil {
		return err
	}
	now := harness.clock.Now()
	policy, err := createFreezePolicy(harness, domain.FreezePolicyInput{
		Zone:     "*",
		Scope:    "activation",
		Reason:   "Global activation hold",
		StartsAt: now.Add(-5 * time.Minute),
		EndsAt:   now.Add(time.Hour),
	})
	if err != nil {
		return err
	}
	// Scope selectivity: revisions are not covered by an activation freeze.
	if err := addRevision(harness, planF.ID, 2026092502, ""); err != nil {
		return fmt.Errorf("activation-scoped freeze blocked a revision: %w", err)
	}
	err = activatePlan(harness, planF.ID, 3, "")
	message, err := expectFreeze(err)
	if err != nil {
		return err
	}
	if !strings.Contains(message, "Global activation hold") {
		return ensure(false, "wildcard freeze did not block activation")
	}
	// The interception is filed under the attempted zone, not the wildcard.
	var page domain.FreezeInterceptionPage
	if err := harness.call(http.MethodGet, "/v1/freeze-interceptions?zone=other.example", nil, &page); err != nil {
		return err
	}
	if len(page.Items) != 1 || page.Items[0].PolicyID != policy.ID {
		return ensure(false, "wildcard interception was not filed under the target zone")
	}
	if _, err := revokeFreezePolicy(harness, policy.ID, 1); err != nil {
		return err
	}
	if _, err := activatePlanOK(harness, planF.ID, 3, ""); err != nil {
		return fmt.Errorf("activation still blocked after wildcard revocation: %w", err)
	}
	return nil
}

func createFreezePolicy(harness *harness, input domain.FreezePolicyInput) (domain.FreezePolicy, error) {
	var policy domain.FreezePolicy
	err := harness.call(http.MethodPut, "/v1/freeze-policies", input, &policy)
	return policy, err
}

func revokeFreezePolicy(harness *harness, policyID string, version uint64) (domain.FreezePolicy, error) {
	var policy domain.FreezePolicy
	err := harness.call(http.MethodPatch, "/v1/freeze-policies/"+policyID+"/revoke", domain.FreezeRevokeInput{
		ExpectedVersion: version,
	}, &policy)
	return policy, err
}

func revokeFreezePolicyErr(harness *harness, policyID string, version uint64) error {
	return harness.call(http.MethodPatch, "/v1/freeze-policies/"+policyID+"/revoke", domain.FreezeRevokeInput{
		ExpectedVersion: version,
	}, nil)
}

func addRevision(harness *harness, planID string, serial uint32, role string) error {
	return harness.callWithHeaders(http.MethodPatch, "/v1/plans/"+planID+"/revisions", domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1.edge.example",
		Entries: []domain.ZoneEntry{
			{Owner: "edge.example", Type: domain.EntryA, Value: "192.0.2.10"},
		},
	}, nil, roleHeaders(role))
}

func activatePlan(harness *harness, planID string, version uint64, role string) error {
	return harness.callWithHeaders(http.MethodPatch, "/v1/plans/"+planID+"/activate", map[string]any{
		"expected_version": version,
	}, nil, roleHeaders(role))
}

func activatePlanOK(harness *harness, planID string, version uint64, role string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := harness.callWithHeaders(http.MethodPatch, "/v1/plans/"+planID+"/activate", map[string]any{
		"expected_version": version,
	}, &plan, roleHeaders(role))
	return plan, err
}

func allocateWithRole(harness *harness, planID string, requestKey string, role string) error {
	return harness.callWithHeaders(http.MethodPatch, "/v1/plans/"+planID+"/leases", domain.LeaseInput{
		RequestKey: requestKey, Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, nil, roleHeaders(role))
}

func allocateOK(harness *harness, planID string, requestKey string, role string) (domain.LeaseGroup, error) {
	var group domain.LeaseGroup
	err := harness.callWithHeaders(http.MethodPatch, "/v1/plans/"+planID+"/leases", domain.LeaseInput{
		RequestKey: requestKey, Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, &group, roleHeaders(role))
	return group, err
}

func roleHeaders(role string) map[string]string {
	if role == "" {
		return nil
	}
	return map[string]string{"X-Actor-Role": role}
}

func expectFreeze(err error) (string, error) {
	var failure *apiFailure
	if !errors.As(err, &failure) {
		if err == nil {
			return "", errors.New("expected a zone_frozen failure but the request succeeded")
		}
		return "", fmt.Errorf("expected a zone_frozen failure, got %v", err)
	}
	if failure.Status != http.StatusConflict || failure.Code != string(domain.CodeZoneFrozen) {
		return "", fmt.Errorf("expected HTTP 409 zone_frozen, got HTTP %d %s", failure.Status, failure.Code)
	}
	return failure.Message, nil
}
