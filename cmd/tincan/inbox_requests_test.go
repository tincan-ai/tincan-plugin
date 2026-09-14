package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCommitmentsApprovalClarificationAndRestart(t *testing.T) {
	c, _ := fixture(t, nil)
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	policy := standingPolicy{Source: "user turn 1", Scope: "Review methodology; dataset access needs approval", Boundaries: []string{"dataset access"}}
	if err = i.policy(policy); err != nil {
		t.Fatal(err)
	}
	if _, err = i.ingest(event(1, "peer", "self"), true); err != nil {
		t.Fatal(err)
	}
	a, err := i.claim(1, "reviewer")
	if err != nil || !a.Acquired {
		t.Fatal(a, err)
	}
	waiting := workerOutcome{Status: "awaiting_approval", Summary: "A/B statistics review", Context: "Review assignment retained", Question: "May I access the experiment dataset?", Permission: "dataset access"}
	if _, err = i.outcome(1, a.Claim, waiting); err != nil {
		t.Fatal(err)
	}
	question := i.state.request(1).Approval.ID
	if _, err = i.outcome(1, a.Claim, waiting); err != nil || i.state.request(1).Approval.ID != question {
		t.Fatal("suspension retry duplicated question", err)
	}
	// Same channel, independent work: no new channel needed and no approval implied.
	_, _ = i.ingest(event(2, "peer", "self"), true)
	b, err := i.claim(2, "responder")
	if err != nil || !b.Acquired {
		t.Fatal("approval blocked another message", b, err)
	}
	if err = i.ack(2, b.Claim); err != nil {
		t.Fatal(err)
	}
	_, _ = i.ingest(event(3, "peer", "self"), true)
	clarification, err := i.claim(3, "context-reader")
	if err != nil || !clarification.Acquired {
		t.Fatal(err)
	}
	if err = i.decision(1, "", "annotate", "peer message 3", "Use the corrected denominator; this does not authorize dataset access", false); err != nil {
		t.Fatal(err)
	}
	if err = i.ack(3, clarification.Claim); err != nil {
		t.Fatal(err)
	}
	if i.state.request(1).Status != "awaiting_approval" || i.state.After != 3 {
		t.Fatal("receipt changed permission or work completion")
	}
	i.close()
	// Offline controller recovery uses the saved identity, not another join.
	i, err = openInboxIdentity(c, []string{"peer"}, configPath(), false, "self", false)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	if i.state.request(1).Approval.Delivery != "pending" {
		t.Fatal("lost undelivered question")
	}
	denied, _ := i.claim(1, "premature")
	if denied.Acquired {
		t.Fatal("claimed before approval")
	}
	if err = i.decision(1, question, "approve", "", "", false); err == nil {
		t.Fatal("accepted approval without user source")
	}
	if err = i.decision(1, "wrong-question", "approve", "user turn 2", "yes", false); err == nil {
		t.Fatal("accepted unrelated approval")
	}
	if err = i.decision(1, question, "presented", "user-facing UI delivery", "", false); err != nil {
		t.Fatal(err)
	}
	if err = i.decision(1, question, "approve", "user turn 2", "Dataset access allowed for this review only", false); err != nil {
		t.Fatal(err)
	}
	resumed, err := i.claim(1, "replacement-after-safe-suspension")
	if err != nil || !resumed.Acquired {
		t.Fatal(resumed, err)
	}
	if resumed.Claim == a.Claim || !strings.Contains(resumed.Context, "corrected denominator") || !strings.Contains(resumed.Context, "this review only") {
		t.Fatal("lost context or reused stale attempt", resumed)
	}
	if resumed.Policy.Scope != policy.Scope || resumed.Policy.Revision != 1 {
		t.Fatal("one-time decision broadened standing policy")
	}
	if err = i.ack(1, a.Claim); err == nil {
		t.Fatal("stale worker completed resumed request")
	}
	if err = i.ack(1, resumed.Claim); err != nil {
		t.Fatal(err)
	}
	if i.state.request(1).Status != "completed" {
		t.Fatal("request not completed")
	}
}

func TestCommitmentDependenciesResourcesAndUncertainRecovery(t *testing.T) {
	i := emptyWaitingInbox(t)
	for seq := int64(1); seq <= 4; seq++ {
		if _, err := i.ingest(event(seq, "peer", "self"), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.plan(1, nil, []string{"file:/project/report"}); err != nil {
		t.Fatal(err)
	}
	if err := i.plan(2, nil, []string{"file:/project/report"}); err != nil {
		t.Fatal(err)
	}
	if err := i.plan(3, []int64{1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := i.plan(1, []int64{3}, nil); err == nil {
		t.Fatal("accepted cyclic dependency")
	}
	a, _ := i.claim(1, "first")
	b, _ := i.claim(2, "conflicting")
	if b.Acquired {
		t.Fatal("shared resource raced")
	}
	dep, _ := i.claim(3, "dependent")
	if dep.Acquired {
		t.Fatal("dependency ignored")
	}
	free, _ := i.claim(4, "unrelated")
	if !free.Acquired {
		t.Fatal("channel used as a lock")
	}
	_, err := i.outcome(1, a.Claim, workerOutcome{Status: "needs_recovery", Context: "external write outcome unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if err = i.decision(1, "", "resume", "operator", "", false); err == nil {
		t.Fatal("uncertain worker automatically released")
	}
	b, _ = i.claim(2, "still-conflicting")
	if b.Acquired {
		t.Fatal("uncertain resource lock lost")
	}
	if err = i.decision(1, "", "resume", "operator confirmed prior execution stopped", "verified no write occurred", true); err != nil {
		t.Fatal(err)
	}
	a, _ = i.claim(1, "recovered")
	if !a.Acquired {
		t.Fatal("recovery failed")
	}
	if err = i.ack(1, a.Claim); err != nil {
		t.Fatal(err)
	}
	b, _ = i.claim(2, "next")
	dep, _ = i.claim(3, "now-dependent")
	if !b.Acquired || !dep.Acquired {
		t.Fatal("completion did not unblock real dependencies")
	}
}
func TestCommitmentLegacyMigrationAndPrivateSnapshots(t *testing.T) {
	c, _ := fixture(t, nil)
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	path := i.path
	i.close()
	e := event(7, "peer", "self")
	legacy := inboxState{After: 6, Pending: &e, Claim: &inboxClaim{WorkerID: "old", Token: "private-token"}}
	data, _ := json.Marshal(legacy)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	i, err = openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	r := i.state.request(7)
	if i.state.Version != 2 || i.state.After != 7 || r.Status != "needs_recovery" || r.Claim.Token != "private-token" {
		t.Fatal("migration lost execution protection", r)
	}
	snapshot, _ := json.Marshal(i.requests())
	if strings.Contains(string(snapshot), "private-token") {
		t.Fatal("snapshot leaked claim token")
	}
	if _, err = i.ingest(event(8, "peer", "self"), true); err != nil {
		t.Fatal(err)
	}
	next, _ := i.claim(8, "new-worker")
	if !next.Acquired {
		t.Fatal("uncertain legacy request blocked independent work")
	}
}
func TestCommitmentDecisionNotificationAndResume(t *testing.T) {
	i := emptyWaitingInbox(t)
	_, _ = i.ingest(event(1, "peer", "self"), true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notices := make(chan map[string]any, 20)
	go i.dispatchChanges(ctx, func(_ context.Context, n map[string]any) error { notices <- n; return nil })
	receive := func(kind string) {
		t.Helper()
		select {
		case n := <-notices:
			if n["kind"] != kind {
				t.Fatal(n)
			}
		case <-time.After(time.Second):
			t.Fatal("missing notice", kind)
		}
	}
	receive("mention")
	a, _ := i.claim(1, "worker")
	_, err := i.outcome(1, a.Claim, workerOutcome{Status: "awaiting_approval", Question: "May I inspect data?"})
	if err != nil {
		t.Fatal(err)
	}
	receive("approval_needed")
	id := i.state.request(1).Approval.ID
	if i.state.request(1).Approval.Delivery != "pending" {
		t.Fatal("transport delivery impersonated human presentation")
	}
	if err = i.decision(1, id, "approve", "user approved", "yes", false); err != nil {
		t.Fatal(err)
	}
	receive("mention")
}
func TestCommitmentOutcomesRejectMalformedAndPreserveClaim(t *testing.T) {
	i := emptyWaitingInbox(t)
	_, _ = i.ingest(event(1, "peer", "self"), true)
	a, _ := i.claim(1, "worker")
	for _, o := range []workerOutcome{{Status: "finished speaking"}, {Status: "awaiting_approval"}, {Status: "awaiting_information"}} {
		if _, err := i.outcome(1, a.Claim, o); err == nil {
			t.Fatal("accepted malformed outcome", o)
		}
	}
	if _, err := i.outcome(1, "", workerOutcome{Status: "completed"}); err == nil {
		t.Fatal("unclaimed completion")
	}
	if i.state.request(1).Claim.Token != a.Claim {
		t.Fatal("invalid outcome discarded claim")
	}
}

func TestCommitmentCheckpointPreservesConcurrentCorrection(t *testing.T) {
	i := emptyWaitingInbox(t)
	_, _ = i.ingest(event(1, "peer", "self"), true)
	a, _ := i.claim(1, "worker")
	if err := i.decision(1, "", "annotate", "peer clarification 2", "Correct denominator is 400", false); err != nil {
		t.Fatal(err)
	}
	if _, err := i.outcome(1, a.Claim, workerOutcome{Status: "awaiting_information", Question: "Which cohort?", Context: "Original analysis checkpoint"}); err != nil {
		t.Fatal(err)
	}
	r := i.state.request(1)
	if !strings.Contains(r.Context, "400") || !strings.Contains(r.Context, "checkpoint") {
		t.Fatal("checkpoint overwrote concurrent correction", r.Context)
	}
}

func TestCommitmentMCPPolicyAndDecisionContract(t *testing.T) {
	i := emptyWaitingInbox(t)
	_, _ = i.ingest(event(1, "peer", "self"), true)
	server := mcp.NewServer(&mcp.Implementation{Name: "controller-test", Version: "1"}, nil)
	addClaimTools(server, func(string) (*inbox, error) { return i, nil })
	a, z := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "host-test", Version: "1"}, nil).Connect(context.Background(), z, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ownerCall(t, client, "inbox_policy_set", map[string]any{"policy": map[string]any{"source": "user turn 1", "scope": "Review only"}})
	claim := ownerCall(t, client, "inbox_claim", map[string]any{"seq": 1, "worker_id": "child"})
	if claim["policy"].(map[string]any)["scope"] != "Review only" {
		t.Fatal("policy omitted from claim")
	}
	result := ownerCall(t, client, "inbox_outcome", map[string]any{"seq": 1, "claim": claim["claim"], "outcome": map[string]any{"status": "awaiting_approval", "question": "May I inspect data?", "context": "saved context"}})
	id := result["approval"].(map[string]any)["id"]
	snapshot := ownerCall(t, client, "inbox_requests", map[string]any{})
	if snapshot["controller_version"] != float64(2) {
		t.Fatal(snapshot)
	}
	ownerCall(t, client, "inbox_decide", map[string]any{"seq": 1, "decision_id": id, "action": "presented", "source": "host UI displayed question"})
	ownerCall(t, client, "inbox_decide", map[string]any{"seq": 1, "decision_id": id, "action": "approve", "source": "user turn 2", "context": "this request only"})
	resumed := ownerCall(t, client, "inbox_claim", map[string]any{"seq": 1, "worker_id": "replacement"})
	if resumed["acquired"] != true || resumed["claim"] == claim["claim"] {
		t.Fatal(resumed)
	}
}

func TestCommitmentNoticeIdentitySurvivesRouting(t *testing.T) {
	for _, kind := range []string{"mention", "approval_needed", "needs_attention", "join_request"} {
		notice := inboundNotification(map[string]any{"kind": kind, "event_seq": int64(1), "notice_key": "new-decision", "payload": "private peer text"})
		if notice["notice_key"] != "new-decision" || notice["payload"] != nil {
			t.Fatal("notice lost identity or leaked peer body", notice)
		}
	}
}

func TestCommitmentFormatRejectsLegacyReaderWithoutLosingNewState(t *testing.T) {
	i := emptyWaitingInbox(t)
	_, err := i.ingest(event(1, "peer", "self"), true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(i.path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		After   int64
		Pending *inboxEvent
		Claim   *inboxClaim
		Reply   *string
	}
	if err = json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	if !((legacy.Claim != nil || legacy.Reply != nil) && legacy.Pending == nil) {
		t.Fatal("old reader could silently overwrite v2 requests")
	}
	var restored inboxState
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if err = restored.migrate(); err != nil {
		t.Fatal(err)
	}
	if r := restored.request(1); r == nil || r.Status != "ready" || r.Claim != nil {
		t.Fatal("format guard changed request ownership")
	}
}

func TestCommitmentWorkerClarificationDoesNotGrantPermission(t *testing.T) {
	i := emptyWaitingInbox(t)
	_, _ = i.ingest(event(1, "peer", "self"), true)
	a, _ := i.claim(1, "reviewer")
	_, err := i.outcome(1, a.Claim, workerOutcome{Status: "awaiting_approval", Summary: "A/B review", Question: "May I read data?"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = i.ingest(event(2, "peer", "self"), true)
	b, _ := i.claim(2, "clarification-reader")
	if len(b.Related) != 1 || b.Related[0].Seq != 1 {
		t.Fatal("worker cannot correlate clarification", b)
	}
	updates := []commitmentUpdate{{Seq: 1, Context: "Correct denominator is 400"}}
	if err = i.contextUpdates(2, b.Claim, updates); err != nil {
		t.Fatal(err)
	}
	if err = i.contextUpdates(2, b.Claim, updates); err != nil {
		t.Fatal(err)
	}
	if strings.Count(i.state.request(1).Context, "400") != 1 {
		t.Fatal("clarification retry duplicated context")
	}
	if _, err = i.outcome(2, b.Claim, workerOutcome{Status: "completed", Updates: updates}); err != nil {
		t.Fatal(err)
	}
	if i.state.request(1).Status != "awaiting_approval" || i.state.request(1).Approval.Source != "" {
		t.Fatal("peer clarification granted permission")
	}
}
