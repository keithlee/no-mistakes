package feedback

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestReconcilerRequiresRepairValidationAndReplyInOrder(t *testing.T) {
	r := New()
	item := scm.FeedbackItem{ID: "comment:7", Kind: scm.FeedbackInlineReview}
	if !r.Reserve(item, "head-1") || r.Reserve(item, "head-1") {
		t.Fatal("Reserve must accept an item exactly once")
	}
	if got := r.Validate("head-2"); len(got) != 0 {
		t.Fatalf("Validate before repair = %+v, want none", got)
	}
	if !r.MarkRepaired(item.ID, "head-2") {
		t.Fatal("MarkRepaired rejected a reserved item")
	}
	ready := r.Validate("head-2")
	if len(ready) != 1 || ready[0].ValidatedHead != "head-2" {
		t.Fatalf("Validate = %+v, want one head-bound record", ready)
	}
	if r.MarkReplied(item.ID, "other-head") || !r.MarkReplied(item.ID, "head-2") {
		t.Fatal("MarkReplied must require the validated head")
	}
}
