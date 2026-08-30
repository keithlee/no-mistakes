package scm

import "testing"

func TestFeedbackPolicyCoversHumanAndConfiguredBotFeedback(t *testing.T) {
	policy := FeedbackPolicy{PRAuthor: "author", IncludeBots: true, BotAuthorPatterns: []string{"*"}}
	for _, item := range []FeedbackItem{
		{Kind: FeedbackInlineReview, Author: "reviewer"},
		{Kind: FeedbackIssueComment, Author: "lint[bot]", AuthorIsBot: true},
		{Kind: FeedbackReview, Author: "reviewer", ReviewState: "CHANGES_REQUESTED", Body: "please revise"},
		{Kind: FeedbackReview, Author: "reviewer", ReviewState: "APPROVED", Body: "looks good"},
	} {
		if !policy.InScope(item) {
			t.Fatalf("item unexpectedly out of scope: %+v", item)
		}
	}
	for _, item := range []FeedbackItem{
		{Kind: FeedbackIssueComment, Author: "author"},
		{Kind: FeedbackReview, Author: "reviewer", ReviewState: "APPROVED"},
	} {
		if policy.InScope(item) {
			t.Fatalf("item unexpectedly in scope: %+v", item)
		}
	}
}

func TestFeedbackDispositionMarkerRoundTripsInsideReplyText(t *testing.T) {
	marker := FeedbackDispositionMarker("comment:17", "abc123", "fixed")
	id, head, disposition, ok := ParseFeedbackDispositionMarker("Addressed and validated.\n\n" + marker)
	if !ok || id != "comment:17" || head != "abc123" || disposition != "fixed" {
		t.Fatalf("parsed marker = %q %q %q %v", id, head, disposition, ok)
	}
}
