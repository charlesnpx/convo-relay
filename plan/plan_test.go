package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPlanRoundTripAndMissingRequiredField(t *testing.T) {
	want := Plan{
		Kind:          PlanKind,
		SchemaVersion: SchemaVersion,
		SessionID:     "public-roundtrip",
		Provenance:    ProvenanceSupplied,
		Task:          "verify the public plan boundary",
		Timeouts:      Timeouts{TurnSeconds: 10, StallSeconds: 5},
		Mode:          ModeCooperative,
		Investigation: InvestigationNormal,
		Actors:        []Actor{{ID: "actor", Backend: ActorBackendCodex}},
		Schedule:      Schedule{Kind: ScheduleSequence, Turns: 1, Order: []string{"actor"}},
		ProviderRetry: ProviderRetry{Mode: ProviderRetryForbid, MaxAttempts: 1},
		Workspace:     Workspace{Mode: WorkspaceModeCurrent},
		Inputs:        []Input{},
		Context:       []Input{},
		Skills:        []Input{},
		ChildPolicy:   ChildPolicy{Mode: ChildPolicyDeny, AllowedRecipes: []string{}},
		Result:        Result{Source: ResultSourceLastTurn, Format: ResultFormatText},
	}
	if err := Validate(want); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	body, err := CanonicalBytes(want)
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	var got Plan
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded plan = %#v, want %#v", got, want)
	}
	wantDigest, err := Digest(want)
	if err != nil {
		t.Fatalf("want digest: %v", err)
	}
	gotDigest, err := Digest(got)
	if err != nil {
		t.Fatalf("got digest: %v", err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("decoded digest = %q, want %q", gotDigest, wantDigest)
	}

	missing := want
	missing.SessionID = ""
	if err := Validate(missing); err == nil || !strings.Contains(err.Error(), "session_id") {
		t.Fatalf("missing session_id error = %v", err)
	}
}

func TestPlanRejectsInstructionForUnscheduledActor(t *testing.T) {
	value := Plan{
		Kind:          PlanKind,
		SchemaVersion: SchemaVersion,
		SessionID:     "instruction-schedule",
		Provenance:    ProvenanceSupplied,
		Task:          "validate instruction ownership",
		Timeouts:      Timeouts{TurnSeconds: 10, StallSeconds: 5},
		Mode:          ModeCooperative,
		Investigation: InvestigationNormal,
		Actors: []Actor{
			{ID: "slot_0", Backend: ActorBackendCodex},
			{ID: "slot_1", Backend: ActorBackendCodex},
		},
		Schedule:      Schedule{Kind: ScheduleSequence, Turns: 2, Order: []string{"slot_0", "slot_1"}},
		ProviderRetry: ProviderRetry{Mode: ProviderRetryForbid, MaxAttempts: 1},
		Workspace:     Workspace{Mode: WorkspaceModeCurrent},
		Inputs:        []Input{},
		Context:       []Input{},
		Skills:        []Input{},
		ChildPolicy:   ChildPolicy{Mode: ChildPolicyDeny, AllowedRecipes: []string{}},
		Result:        Result{Source: ResultSourceLastTurn, Format: ResultFormatText},
		Instructions: &Instructions{Turns: []TurnInstruction{{
			ParticipantTurn: 1,
			Actor:           "slot_1",
			Instructions:    "reply with BANANA",
		}}},
	}
	err := Validate(value)
	if err == nil || !strings.Contains(err.Error(), "participant turn 1") || !strings.Contains(err.Error(), "slot_1") || !strings.Contains(err.Error(), "slot_0") {
		t.Fatalf("unscheduled instruction error = %v, want turn and both actors", err)
	}
}

func TestPlanRejectsFacilitatorOnSequenceSchedule(t *testing.T) {
	value := Plan{
		Kind:          PlanKind,
		SchemaVersion: SchemaVersion,
		SessionID:     "sequence-facilitator",
		Provenance:    ProvenanceSupplied,
		Task:          "validate sequence controls",
		Timeouts:      Timeouts{TurnSeconds: 10, StallSeconds: 5},
		Mode:          ModeCooperative,
		Investigation: InvestigationNormal,
		Actors: []Actor{
			{ID: "participant", Backend: ActorBackendCodex},
			{ID: "facilitator", Backend: ActorBackendCodex},
		},
		Schedule:    Schedule{Kind: ScheduleSequence, Turns: 1, Order: []string{"participant"}},
		Facilitator: &Facilitator{Actor: "facilitator", Cadence: 1},
		ProviderRetry: ProviderRetry{
			Mode:        ProviderRetryForbid,
			MaxAttempts: 1,
		},
		Workspace:   Workspace{Mode: WorkspaceModeCurrent},
		Inputs:      []Input{},
		Context:     []Input{},
		Skills:      []Input{},
		ChildPolicy: ChildPolicy{Mode: ChildPolicyDeny, AllowedRecipes: []string{}},
		Result:      Result{Source: ResultSourceLastTurn, Format: ResultFormatText},
	}
	if err := Validate(value); err == nil || err.Error() != "sequence schedule must not declare a facilitator" {
		t.Fatalf("sequence facilitator error = %v, want sequence schedule rejection", err)
	}
}
