package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// newAxiPRReadinessCmd exposes a stable, read-only readiness decision for
// Firstmate and other callers. It performs a fresh provider read every time.
func newAxiPRReadinessCmd() *cobra.Command {
	var prURL, head, phase string
	cmd := &cobra.Command{
		Use:           "pr-readiness",
		Short:         "Read current pull-request handback or merge readiness",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAxiPRReadiness(cmd, prURL, head, phase)
		},
	}
	cmd.Flags().StringVar(&prURL, "pr", "", "pull request URL")
	cmd.Flags().StringVar(&head, "head", "", "expected pull request head SHA")
	cmd.Flags().StringVar(&phase, "phase", "handback", "readiness phase: handback or merge")
	_ = cmd.MarkFlagRequired("pr")
	_ = cmd.MarkFlagRequired("head")
	return cmd
}

func runAxiPRReadiness(cmd *cobra.Command, prURL, expectedHead, phase string) error {
	if strings.TrimSpace(prURL) == "" || strings.TrimSpace(expectedHead) == "" {
		return emitError(cmd, 2, "--pr and --head are required")
	}
	var readinessPhase scm.ReadinessPhase
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case string(scm.ReadinessHandback):
		readinessPhase = scm.ReadinessHandback
	case string(scm.ReadinessMerge):
		readinessPhase = scm.ReadinessMerge
	default:
		return emitError(cmd, 2, "--phase must be handback or merge")
	}
	provider := scm.DetectProvider(prURL)
	if provider != scm.ProviderGitHub {
		result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, ProviderSupported: false, StateReadable: false})
		return emitReadiness(cmd, prURL, readinessPhase, result)
	}
	repo := github.RepoSlug(prURL)
	hostName := scm.ExtractHost(prURL)
	host := github.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, name, args...)
	}, func() bool { _, err := exec.LookPath("gh"); return err == nil }, hostName, repo)
	pr := &scm.PR{URL: prURL, HeadSHA: expectedHead}
	snapshot, err := host.GetFeedback(cmd.Context(), pr)
	if err != nil {
		result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, ProviderSupported: true, StateReadable: false})
		result.Reason = err.Error()
		return emitReadiness(cmd, prURL, readinessPhase, result)
	}
	checks, err := host.GetChecks(cmd.Context(), pr)
	if err != nil {
		result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, ProviderSupported: true, StateReadable: false})
		result.Reason = err.Error()
		return emitReadiness(cmd, prURL, readinessPhase, result)
	}
	ciReady := len(checks) > 0
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketPass && check.Bucket != scm.CheckBucketSkip {
			ciReady = false
			break
		}
	}
	policy := scm.FeedbackPolicy{PRAuthor: snapshot.PRAuthor, IncludeBots: true, BotAuthorPatterns: []string{"*"}}
	if p, pathErr := paths.New(); pathErr == nil {
		if global, configErr := config.LoadGlobal(p.ConfigFile()); configErr == nil {
			policy.IncludeBots = global.Feedback.IncludeBots == nil || *global.Feedback.IncludeBots
			if len(global.Feedback.BotAuthorPatterns) > 0 {
				policy.BotAuthorPatterns = append([]string(nil), global.Feedback.BotAuthorPatterns...)
			}
		}
	}
	var unresolvedIDs, unresolvedURLs []string
	for _, item := range snapshot.Items {
		if item.Resolved || !policy.InScope(item) || markerAddresses(item, snapshot.HeadSHA) {
			continue
		}
		unresolvedIDs = append(unresolvedIDs, item.ID)
		if item.URL != "" {
			unresolvedURLs = append(unresolvedURLs, item.URL)
		}
	}
	result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, CurrentHead: snapshot.HeadSHA, CIReady: ciReady, ProofReviewPassed: proofReviewForCurrentHead(prURL, expectedHead), ReviewDecision: snapshot.ReviewDecision, UnresolvedIDs: unresolvedIDs, UnresolvedURLs: unresolvedURLs, ProviderSupported: true, StateReadable: true})
	return emitReadiness(cmd, prURL, readinessPhase, result)
}

func markerAddresses(item scm.FeedbackItem, head string) bool {
	markerID, markerHead, disposition, ok := scm.ParseFeedbackDispositionMarker(item.Body)
	return ok && strings.TrimSpace(markerID) == strings.TrimSpace(item.ID) && strings.EqualFold(markerHead, strings.TrimSpace(head)) && strings.TrimSpace(disposition) != ""
}

// proofReviewForCurrentHead reads the local run ledger and accepts proof only
// when the exact PR and head have a completed proof-review step. A missing or
// unreadable local ledger is deliberately false: provider feedback alone is
// never proof that the independent gate ran.
func proofReviewForCurrentHead(prURL, head string) bool {
	env, err := openAxiEnv(false)
	if err != nil {
		return false
	}
	defer env.close()
	if env.repo == nil {
		return false
	}
	runs, err := env.d.GetRunsByRepo(env.repo.ID)
	if err != nil {
		return false
	}
	for _, run := range runs {
		if run == nil || run.PRURL == nil || !strings.EqualFold(strings.TrimSpace(*run.PRURL), strings.TrimSpace(prURL)) || !strings.EqualFold(strings.TrimSpace(run.HeadSHA), strings.TrimSpace(head)) {
			continue
		}
		steps, err := env.d.GetStepsByRun(run.ID)
		if err != nil {
			return false
		}
		for _, step := range steps {
			if step.StepName == types.StepProofReview && step.Status == types.StepStatusCompleted {
				return true
			}
		}
	}
	return false
}

func emitReadiness(cmd *cobra.Command, prURL string, phase scm.ReadinessPhase, result scm.ReadinessResult) error {
	fields := []toon.Field{{Key: "pr", Value: prURL}, {Key: "phase", Value: string(phase)}, {Key: "ready", Value: result.Ready}, {Key: "head", Value: result.Head}, {Key: "proof_review", Value: result.ProofReview}, {Key: "ci", Value: result.CI}, {Key: "review_decision", Value: result.ReviewDecision}, {Key: "unresolved_item_ids", Value: result.UnresolvedIDs}, {Key: "unresolved_item_urls", Value: result.UnresolvedURLs}, {Key: "unknown", Value: result.Unknown}, {Key: "reason", Value: result.Reason}}
	emitDoc(cmd, fields...)
	if result.Ready {
		return nil
	}
	return &exitError{code: 1, err: fmt.Errorf("PR is not ready: %s", result.Reason)}
}
