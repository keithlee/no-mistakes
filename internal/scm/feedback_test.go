package scm

import "testing"

func TestFeedbackDispositionMarkerBindsSourceAndHead(t *testing.T) {
	marker := FeedbackDispositionMarker("comment 42", "abc123", "repaired")
	id, head, disposition, ok := ParseFeedbackDispositionMarker(marker)
	if !ok || id != "comment 42" || head != "abc123" || disposition != "repaired" {
		t.Fatalf("parsed marker = %q, %q, %q, %v", id, head, disposition, ok)
	}
	if _, _, _, ok := ParseFeedbackDispositionMarker(FeedbackDispositionMarker("comment 42", "other", "repaired")); !ok {
		t.Fatal("valid marker did not parse")
	}
	if _, _, _, ok := ParseFeedbackDispositionMarker("<!-- no-mistakes: feedback-disposition !!! abc repaired -->"); ok {
		t.Fatal("malformed marker should not parse")
	}
}

func TestFeedbackPolicyExcludesAuthorAndUnconfiguredBots(t *testing.T) {
	policy := FeedbackPolicy{PRAuthor: "owner", IncludeBots: true, BotAuthorPatterns: []string{"reviewer*"}}
	if policy.InScope(FeedbackItem{ID: "1", Author: "owner"}) {
		t.Fatal("PR author's reply was in scope")
	}
	if policy.InScope(FeedbackItem{ID: "2", Author: "other-bot", AuthorIsBot: true}) {
		t.Fatal("unconfigured bot was in scope")
	}
	if !policy.InScope(FeedbackItem{ID: "3", Author: "reviewer[bot]", AuthorIsBot: true}) {
		t.Fatal("configured bot was not in scope")
	}
}

func TestEvaluatePRReadinessPhaseDifference(t *testing.T) {
	base := ReadinessInput{ExpectedHead: "abc", CurrentHead: "abc", CIReady: true, ProofReviewPassed: true, ReviewDecision: "CHANGES_REQUESTED", ProviderSupported: true, StateReadable: true}
	if !EvaluatePRReadiness(ReadinessInput{Phase: ReadinessHandback, ExpectedHead: base.ExpectedHead, CurrentHead: base.CurrentHead, CIReady: true, ProofReviewPassed: true, ReviewDecision: base.ReviewDecision, ProviderSupported: true, StateReadable: true}).Ready {
		t.Fatal("handback should allow handing back to a reviewer")
	}
	merge := EvaluatePRReadiness(ReadinessInput{Phase: ReadinessMerge, ExpectedHead: base.ExpectedHead, CurrentHead: base.CurrentHead, CIReady: true, ProofReviewPassed: true, ReviewDecision: base.ReviewDecision, ProviderSupported: true, StateReadable: true})
	if merge.Ready || merge.Reason != "review decision is CHANGES_REQUESTED" {
		t.Fatalf("merge readiness = %+v", merge)
	}
}
