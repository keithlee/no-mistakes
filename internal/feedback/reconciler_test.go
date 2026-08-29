package feedback

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestReconcilerRequiresObjectiveDecisionAndRestarts(t *testing.T) {
	r := New(2)
	item := scm.FeedbackItem{ID: "c1", Kind: scm.FeedbackIssueComment, Author: "reviewer", Body: "please fix"}
	s := scm.FeedbackSnapshot{HeadSHA: "abc", Items: []scm.FeedbackItem{item}}
	policy := scm.FeedbackPolicy{PRAuthor: "author"}
	if got := r.Observe(s, policy, "abc", map[string]Decision{"c1": {Classification: AskUser}}); got.Action != Blocked {
		t.Fatalf("ask-user action = %s", got.Action)
	}
	got := r.Observe(s, policy, "abc", map[string]Decision{"c1": {Classification: Objective, ReplyBody: "Validated fix on abc"}})
	if got.Action != Restart || got.Attempt != 1 {
		t.Fatalf("objective action = %+v", got)
	}
	if got := r.Observe(s, policy, "abc", map[string]Decision{"c1": {Classification: Objective}}); got.Action != NoAction {
		t.Fatalf("duplicate action = %+v", got)
	}
}

func TestReconcilerNeverRepliesBeforeValidationAndResolvesAfterReply(t *testing.T) {
	r := New(1)
	item := scm.FeedbackItem{ID: "c1", ThreadID: "t1", Kind: scm.FeedbackInlineReview, Author: "reviewer"}
	r.Observe(scm.FeedbackSnapshot{HeadSHA: "abc", Items: []scm.FeedbackItem{item}}, scm.FeedbackPolicy{}, "abc", map[string]Decision{"c1": {Classification: Objective, ReplyBody: "fixed abc"}})
	if got := r.ValidationPassed("abc", false, true); got != nil {
		t.Fatalf("replied before CI: %+v", got)
	}
	actions := r.ValidationPassed("abc", true, true)
	if len(actions) != 1 || actions[0].Action != Reply {
		t.Fatalf("validation actions = %+v", actions)
	}
	if got, ok := r.Disposition("c1", "abc", false); ok || got.Action != Blocked {
		t.Fatalf("unreplied disposition = %+v, %v", got, ok)
	}
	if got, ok := r.Disposition("c1", "abc", true); !ok || got.Action != Resolve {
		t.Fatalf("resolved disposition = %+v, %v", got, ok)
	}
}

func TestReconcilerRetriesAndRejectsStaleHead(t *testing.T) {
	r := New(2)
	item := scm.FeedbackItem{ID: "c1", Kind: scm.FeedbackIssueComment, Author: "reviewer"}
	if got := r.Observe(scm.FeedbackSnapshot{HeadSHA: "old", Items: []scm.FeedbackItem{item}}, scm.FeedbackPolicy{}, "new", nil); got.Action != Blocked {
		t.Fatalf("stale = %+v", got)
	}
	r.Observe(scm.FeedbackSnapshot{HeadSHA: "new", Items: []scm.FeedbackItem{item}}, scm.FeedbackPolicy{}, "new", map[string]Decision{"c1": {Classification: Objective}})
	if got := r.Retry("c1"); got.Action != Restart || got.Attempt != 2 {
		t.Fatalf("retry = %+v", got)
	}
	if got := r.Retry("c1"); got.Action != Blocked {
		t.Fatalf("exhausted = %+v", got)
	}
}

func TestReconcilerHonorsValidatedAuthorMarkerWithoutRequeue(t *testing.T) {
	r := New(1)
	item := scm.FeedbackItem{ID: "c1", Kind: scm.FeedbackIssueComment, Author: "reviewer"}
	marker := scm.FeedbackItem{ID: "reply", Kind: scm.FeedbackIssueComment, Author: "author", Body: scm.FeedbackDispositionMarker("c1", "abc", "fixed")}
	snapshot := scm.FeedbackSnapshot{HeadSHA: "abc", PRAuthor: "author", Items: []scm.FeedbackItem{item, marker}}
	got := r.Observe(snapshot, scm.FeedbackPolicy{PRAuthor: "author"}, "abc", nil)
	if got.Action != NoAction {
		t.Fatalf("marker-dispositioned item was requeued: %+v", got)
	}
}
