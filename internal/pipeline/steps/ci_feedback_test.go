package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/feedback"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type feedbackActionTestHost struct {
	scm.Host
	head     string
	replies  []string
	resolved []string
}

func (h *feedbackActionTestHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{Feedback: true}
}

func (h *feedbackActionTestHost) GetPRState(context.Context, *scm.PR) (scm.PRState, error) {
	return scm.PRStateOpen, nil
}

func (h *feedbackActionTestHost) GetChecks(context.Context, *scm.PR) ([]scm.Check, error) {
	return []scm.Check{{Name: "test", Bucket: scm.CheckBucketPass}}, nil
}

func (h *feedbackActionTestHost) GetFeedback(_ context.Context, _ *scm.PR) (scm.FeedbackSnapshot, error) {
	return scm.FeedbackSnapshot{HeadSHA: h.head, PRAuthor: "author", ViewerLogin: "author"}, nil
}

func (h *feedbackActionTestHost) ReplyToFeedback(_ context.Context, _ *scm.PR, item scm.FeedbackItem, body string) error {
	h.replies = append(h.replies, item.ID+":"+body)
	return nil
}

func (h *feedbackActionTestHost) ResolveFeedback(_ context.Context, _ *scm.PR, item scm.FeedbackItem) error {
	h.resolved = append(h.resolved, item.ID)
	return nil
}

func TestFinalizeHandoffProofPassesBeforeReplyAndResolution(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		switch opts.Purpose {
		case "handoff-proof-review":
			return &agent.Result{Output: []byte(`{"verdict":"pass","summary":"current head proven","findings":[]}`)}, nil
		case "feedback-classification":
			return &agent.Result{Output: []byte(`{"classification":"objective","reply_body":"Addressed and validated on the current head."}`)}, nil
		default:
			t.Fatalf("unexpected agent purpose %q", opts.Purpose)
			return nil, nil
		}
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.StrictHandoff = true
	testStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	evidence := `{"findings":[],"summary":"tests passed","tested":["go test ./..."],"testing_summary":"Focused tests passed.","artifacts":[]}`
	if err := sctx.DB.SetStepFindings(testStep.ID, evidence); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(testStep.ID, 0, 1, "test.log"); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("# rigorous output proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := scm.FeedbackItem{ID: "comment:7", ThreadID: "thread-7", Kind: scm.FeedbackInlineReview, Author: "reviewer"}
	step := &CIStep{proofSkillPath: skillPath, feedbackReconciler: feedback.New(), feedbackDecisions: map[string]feedback.Decision{}}
	step.feedbackReconciler.Reserve(item, baseSHA)
	step.feedbackReconciler.MarkRepaired(item.ID, headSHA)
	host := &feedbackActionTestHost{head: headSHA}
	outcome, ready, err := step.finalizeHandoff(sctx, host, &scm.PR{Number: "7"})
	if err != nil || outcome != nil || !ready {
		t.Fatalf("finalizeHandoff = %#v, %v, %v", outcome, ready, err)
	}
	if len(host.replies) != 1 || len(host.resolved) != 1 || host.resolved[0] != item.ID {
		t.Fatalf("provider actions replies=%v resolved=%v", host.replies, host.resolved)
	}
	if records, err := sctx.DB.GetFeedbackRecords(sctx.Run.ID); err != nil || len(records) != 0 {
		t.Fatalf("retired ledger = %+v, %v", records, err)
	}
}

func TestEnsureProofReviewBlocksMalformedAgentOutput(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"verdict":"pass"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.StrictHandoff = true
	testStep, _ := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	_ = sctx.DB.SetStepFindings(testStep.ID, `{"findings":[],"summary":"ok","tested":["go test"],"testing_summary":"ok","artifacts":[]}`)
	_ = sctx.DB.CompleteStep(testStep.ID, 0, 1, "test.log")
	skillPath := filepath.Join(t.TempDir(), "SKILL.md")
	_ = os.WriteFile(skillPath, []byte("proof"), 0o600)
	outcome, err := (&CIStep{proofSkillPath: skillPath}).ensureProofReview(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("ensureProofReview = %#v, %v, want blocking gate", outcome, err)
	}
}
