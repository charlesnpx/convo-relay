package model

import "testing"

func TestProposalAndAdmissionDecisionCopyMaps(t *testing.T) {
	rawProposal := map[string]any{"proposal_id": "sp_1", "status": "proposed"}
	proposal := NewProposal(rawProposal)
	rawProposal["status"] = "mutated"
	next := proposal.With("status", "admitted")

	if proposal.Status() != "proposed" || next.Status() != "admitted" {
		t.Fatalf("proposal statuses original=%q next=%q", proposal.Status(), next.Status())
	}

	rawDecision := map[string]any{"proposal_id": "sp_1", "decision": "admit"}
	decision := NewAdmissionDecision(rawDecision)
	rawDecision["proposal_id"] = "mutated"
	if decision.ProposalID() != "sp_1" {
		t.Fatalf("decision proposal_id = %q", decision.ProposalID())
	}
}
