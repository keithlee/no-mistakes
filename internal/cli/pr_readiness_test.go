package cli

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestMarkerAddressesRequiresAuthorAndSourceBinding(t *testing.T) {
	item := scm.FeedbackItem{ID: "42", Author: "reviewer", Body: "please fix"}
	marker := scm.FeedbackItem{ID: "99", Author: "owner", Kind: scm.FeedbackIssueComment, Body: scm.FeedbackDispositionMarker("42", "abc", "repaired")}
	if !markerAddresses(item, []scm.FeedbackItem{item, marker}, "owner", "abc") {
		t.Fatal("bound PR-author marker did not address source feedback")
	}
	spoof := marker
	spoof.Author = "reviewer"
	if markerAddresses(item, []scm.FeedbackItem{item, spoof}, "owner", "abc") {
		t.Fatal("external marker addressed feedback")
	}
	wrongSource := marker
	wrongSource.Body = scm.FeedbackDispositionMarker("other", "abc", "repaired")
	if markerAddresses(item, []scm.FeedbackItem{item, wrongSource}, "owner", "abc") {
		t.Fatal("marker for another source addressed feedback")
	}
}
