package workflowprobe

import (
	"fmt"
	"net/http"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// runChangeFreezeEnforcement exercises the change freeze policy across
// the revision, activation and allocation entries, including role
// exceptions, grandfathering, overlap, cross-time-zone boundaries and
// the non-retroactive nature of revocation.
func runChangeFreezeEnforcement() error {
	harness, err := newHarness()
	if err != nil {
		return err
	}
	defer harness.close()

	base := harness.clock.Now()

	for index := 0; index < 3; index++ {
		if _, err := enrollNode(harness, fmt.Sprintf("resolver-east-%02d", index), "east"); err != nil {
			return err
		}
	}

	// F1 freezes edge.example revision+activation from T+1h for 4h.
	var f1 domain.ChangeFreeze
	if err := harness.call(http.MethodPut, "/v1/freezes", domain.FreezeCreateInput{
		Zone:         "edge.example",
		Scopes:       []string{"revision", "activation"},
		StartsAt:     base.Add(time.Hour).Format(time.RFC3339),
		EndsAt:       base.Add(5 * time.Hour).Format(time.RFC3339),
		Reason:       "major event window",
		AllowedRoles: []string{"emergency-admin"},
		CreatedBy:    "ops-lead",
	}, &f1); err != nil {
		return err
	}
	if f1.Status != domain.FreezeStatusScheduled {
		return ensure(false, "future freeze should be scheduled")
	}

	// A draft created before the freeze may keep appending revisions
	// (grandfathered in-flight preparation).
	var oldDraft domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "edge.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &oldDraft); err != nil {
		return err
	}
	var oldRevision domain.ZoneRevision
	if err := harness.call(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/revisions", revisionBody(2026092501, "192.0.2.10"), &oldRevision); err != nil {
		return err
	}

	// Advance into the freeze window.
	harness.clock.Advance(time.Hour + time.Minute)

	var grandfatheredRevision domain.ZoneRevision
	if err := harness.call(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/revisions", revisionBody(2026092502, "192.0.2.11"), &grandfatheredRevision); err != nil {
		return fmt.Errorf("pre-freeze draft revision append should be grandfathered: %w", err)
	}

	// Activation is never grandfathered.
	if err := harness.call(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/activate", map[string]any{
		"expected_version": 3,
	}, nil); err != nil {
		if fErr := expectFailure(err, http.StatusLocked, string(domain.CodeChangeFrozen)); fErr != nil {
			return fErr
		}
	} else {
		return fmt.Errorf("activation during freeze should be blocked")
	}

	// Allowed role bypasses the same freeze.
	if err := harness.callWithHeaders(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/activate", map[string]any{
		"expected_version": 3,
	}, &oldDraft, map[string]string{"X-Operator-Roles": "onlooker, Emergency-Admin"}); err != nil {
		return fmt.Errorf("allowed role should bypass freeze: %w", err)
	}
	if oldDraft.State != domain.PlanActive {
		return ensure(false, "plan should be active after role exception")
	}

	// Allocation scope is not part of F1, so leasing still works on this
	// plan (which is also pre-freeze active).
	var group1 domain.LeaseGroup
	if err := harness.call(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/leases", domain.LeaseInput{
		RequestKey: "freeze-lease-0001", Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, &group1); err != nil {
		return fmt.Errorf("allocation outside freeze scope should proceed: %w", err)
	}

	// A draft created after the freeze started cannot append revisions.
	var newDraft domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "edge.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &newDraft); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+newDraft.ID+"/revisions", revisionBody(2026092503, "192.0.2.12"), nil); err != nil {
		if fErr := expectFailure(err, http.StatusLocked, string(domain.CodeChangeFrozen)); fErr != nil {
			return fErr
		}
	} else {
		return fmt.Errorf("post-freeze draft revision append should be blocked")
	}

	// F2 global freeze covering allocation starts one minute later. The
	// existing plan was activated before F2 (grandfathered for F2) and is
	// outside F1's allocation scope, so allocation still succeeds.
	harness.clock.Advance(time.Minute)
	var f2 domain.ChangeFreeze
	if err := harness.call(http.MethodPut, "/v1/freezes", domain.FreezeCreateInput{
		Zone:     "",
		Scopes:   []string{"allocation"},
		StartsAt: harness.clock.Now().Format(time.RFC3339),
		Reason:   "global allocation hold",
	}, &f2); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/leases", domain.LeaseInput{
		RequestKey: "freeze-lease-0002", Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, nil); err != nil {
		return fmt.Errorf("pre-F2 active plan allocation should be grandfathered: %w", err)
	}

	// Idempotent replay of the blocked-scope behavior is unaffected: the
	// first lease group replays with its original key.
	var replay domain.LeaseGroup
	if err := harness.call(http.MethodPatch, "/v1/plans/"+oldDraft.ID+"/leases", domain.LeaseInput{
		RequestKey: "freeze-lease-0001", Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, &replay); err != nil {
		return fmt.Errorf("idempotent replay should bypass freeze gate: %w", err)
	}
	if replay.ID != group1.ID {
		return ensure(false, "replay should return the original lease group")
	}

	// A plan activated while F2 is in effect is not grandfathered. Create
	// the plan in a zone without an activation freeze, activate it, then
	// attempt allocation: F2 blocks it.
	var otherPlan domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "other.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &otherPlan); err != nil {
		return err
	}
	var otherRevision domain.ZoneRevision
	if err := harness.call(http.MethodPatch, "/v1/plans/"+otherPlan.ID+"/revisions", revisionBody(2026092510, "192.0.2.20"), &otherRevision); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+otherPlan.ID+"/activate", map[string]any{
		"expected_version": 2,
	}, &otherPlan); err != nil {
		return fmt.Errorf("activation outside F1 zone should proceed: %w", err)
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+otherPlan.ID+"/leases", domain.LeaseInput{
		RequestKey: "freeze-lease-other-1", Region: "east", LeaseSeconds: 600, MaxNodes: 1,
	}, nil); err != nil {
		if fErr := expectFailure(err, http.StatusLocked, string(domain.CodeChangeFrozen)); fErr != nil {
			return fErr
		}
	} else {
		return fmt.Errorf("allocation under global freeze should be blocked")
	}

	// Cross-time-zone boundary: a freeze registered with a +09:00 offset
	// is compared on its absolute UTC instant, not the local wall-clock
	// date. The half-open interval [start, end) blocks at the first
	// instant and releases exactly at the end.
	startJST := harness.clock.Now().Add(2 * time.Minute)
	jst := time.FixedZone("JST", 9*60*60)
	formatJST := func(value time.Time) string {
		return value.In(jst).Format(time.RFC3339)
	}
	var f3 domain.ChangeFreeze
	if err := harness.call(http.MethodPut, "/v1/freezes", domain.FreezeCreateInput{
		Zone:     "pacific.example",
		Scopes:   []string{"activation"},
		StartsAt: formatJST(startJST),
		EndsAt:   formatJST(startJST.Add(time.Second)),
		Reason:   "offset boundary check",
	}, &f3); err != nil {
		return err
	}
	var pacificPlan domain.RolloutPlan
	if err := harness.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone: "pacific.example", MinimumSuccessRatio: 1, MaximumLatencyMS: 80, ExpectedNodes: 1,
	}, &pacificPlan); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+pacificPlan.ID+"/revisions", revisionBody(2026092520, "192.0.2.30"), &domain.ZoneRevision{}); err != nil {
		return err
	}
	harness.clock.Advance(2 * time.Minute)
	if err := harness.call(http.MethodPatch, "/v1/plans/"+pacificPlan.ID+"/activate", map[string]any{
		"expected_version": 2,
	}, nil); err != nil {
		if fErr := expectFailure(err, http.StatusLocked, string(domain.CodeChangeFrozen)); fErr != nil {
			return fErr
		}
	} else {
		return fmt.Errorf("activation at the freeze start instant should block")
	}
	harness.clock.Advance(time.Second)
	if err := harness.call(http.MethodPatch, "/v1/plans/"+pacificPlan.ID+"/activate", map[string]any{
		"expected_version": 2,
	}, &pacificPlan); err != nil {
		return fmt.Errorf("activation at the end instant should be released: %w", err)
	}

	// Refresh F3 status through the list endpoint and confirm expiry.
	var expiredPage domain.FreezePage
	if err := harness.call(http.MethodGet, "/v1/freezes?status=expired", nil, &expiredPage); err != nil {
		return err
	}
	if !containsFreeze(expiredPage.Items, f3.ID, domain.FreezeStatusExpired) {
		return ensure(false, "F3 should be reported as expired")
	}

	// Interception records persisted and survive revocation.
	var blocks domain.FreezeBlockPage
	if err := harness.call(http.MethodGet, "/v1/freeze-blocks", nil, &blocks); err != nil {
		return err
	}
	if len(blocks.Items) < 3 {
		return ensure(false, fmt.Sprintf("expected at least 3 freeze blocks, got %d", len(blocks.Items)))
	}
	if !blockCovers(blocks.Items, newDraft.ID, domain.FreezeScopeRevision, f1.ID) {
		return ensure(false, "missing F1 revision block record")
	}
	if !blockCovers(blocks.Items, otherPlan.ID, domain.FreezeScopeAllocation, f2.ID) {
		return ensure(false, "missing F2 allocation block record")
	}

	// Revocation is not retroactive: revoke F1 and confirm its blocks
	// remain while new revisions are allowed again.
	var revoked domain.ChangeFreeze
	if err := harness.call(http.MethodPatch, "/v1/freezes/"+f1.ID+"/revoke", domain.FreezeRevokeInput{
		ExpectedVersion: 1, RevokedBy: "ops-lead", Note: "event ended early",
	}, &revoked); err != nil {
		return err
	}
	if revoked.Status != domain.FreezeStatusRevoked {
		return ensure(false, "F1 should be revoked")
	}
	if err := harness.call(http.MethodPatch, "/v1/plans/"+newDraft.ID+"/revisions", revisionBody(2026092503, "192.0.2.12"), &domain.ZoneRevision{}); err != nil {
		return fmt.Errorf("revision append should work after revocation: %w", err)
	}
	var blocksAfter domain.FreezeBlockPage
	if err := harness.call(http.MethodGet, "/v1/freeze-blocks?freeze_id="+f1.ID, nil, &blocksAfter); err != nil {
		return err
	}
	if len(blocksAfter.Items) == 0 {
		return ensure(false, "revocation must not erase recorded interceptions")
	}
	if err := harness.call(http.MethodPatch, "/v1/freezes/"+f1.ID, domain.FreezeUpdateInput{
		ExpectedVersion: ptrVersion(2),
		Reason:          ptrString("changed note"),
		UpdatedBy:       "ops-lead",
	}, nil); err != nil {
		if fErr := expectFailure(err, http.StatusConflict, string(domain.CodeInvalidState)); fErr != nil {
			return fErr
		}
	} else {
		return fmt.Errorf("revoked freeze must not be updatable")
	}

	// Concurrent update protection: stale expected_version conflicts.
	var f2b domain.ChangeFreeze
	if err := harness.call(http.MethodPatch, "/v1/freezes/"+f2.ID, domain.FreezeUpdateInput{
		ExpectedVersion: ptrVersion(1),
		Reason:          ptrString("extended hold"),
		UpdatedBy:       "ops-lead",
	}, &f2b); err != nil {
		return err
	}
	if err := harness.call(http.MethodPatch, "/v1/freezes/"+f2.ID, domain.FreezeUpdateInput{
		ExpectedVersion: ptrVersion(1),
		Reason:          ptrString("stale write"),
		UpdatedBy:       "ops-lead",
	}, nil); err != nil {
		if fErr := expectFailure(err, http.StatusConflict, string(domain.CodeVersionConflict)); fErr != nil {
			return fErr
		}
	} else {
		return fmt.Errorf("stale freeze version should conflict")
	}
	if f2b.Reason != "extended hold" || f2b.Version != 2 {
		return ensure(false, "freeze update should apply and bump version")
	}

	// Unconfigured zones keep existing behavior: a zone with no matching
	// freeze completes a full lifecycle without any freeze error.
	freePlan, _, err := createActivePlan(harness, "unfrozen.example", 2026092530, 1, 1, 80)
	if err != nil {
		return fmt.Errorf("unfrozen zone lifecycle should be unaffected: %w", err)
	}
	if freePlan.State != domain.PlanActive {
		return ensure(false, "unfrozen zone plan should activate")
	}
	return nil
}

func revisionBody(serial uint32, ip string) domain.RevisionCreateInput {
	return domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1.edge.example",
		Entries: []domain.ZoneEntry{
			{Owner: "edge.example", Type: domain.EntryA, Value: ip},
		},
	}
}

func containsFreeze(items []domain.ChangeFreeze, id string, status domain.FreezeStatus) bool {
	for _, item := range items {
		if item.ID == id && item.Status == status {
			return true
		}
	}
	return false
}

func blockCovers(items []domain.FreezeBlock, planID string, scope domain.FreezeScope, freezeID string) bool {
	for _, item := range items {
		if item.PlanID == planID && item.Scope == scope && item.FreezeID == freezeID {
			return true
		}
	}
	return false
}

func ptrVersion(value uint64) *uint64 {
	return &value
}

func ptrString(value string) *string {
	return &value
}
