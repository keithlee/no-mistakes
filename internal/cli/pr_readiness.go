package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
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
	// Readiness must use the same isolated forge profile as the daemon. Resolve
	// it from trusted global config before constructing any provider command;
	// never let ambient GH_CONFIG_DIR or login state choose the account.
	var forgeEnv []string
	if env, envErr := openAxiEnv(false); envErr == nil {
		defer env.close()
		if env.cfg != nil && len(env.cfg.ForgeProfiles) > 0 {
			upstream := prURL
			fork := ""
			if env.repo != nil {
				upstream, fork = env.repo.UpstreamURL, env.repo.ForkURL
			}
			resolved, resolveErr := forgecontext.Resolve(cmd.Context(), env.cfg.ForgeProfiles, upstream, fork)
			if resolveErr != nil {
				result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, ProviderSupported: true, StateReadable: false})
				result.Reason = "forge profile is unreadable: " + resolveErr.Error()
				return emitReadiness(cmd, prURL, readinessPhase, result)
			}
			if resolved != nil {
				forgeEnv = resolved.Environment.Apply(nil)
			}
		}
	}
	host := github.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		command := exec.CommandContext(ctx, name, args...)
		if forgeEnv != nil {
			command.Env = forgeEnv
		}
		return command
	}, func() bool { _, err := exec.LookPath("gh"); return err == nil }, hostName, repo)
	pr := &scm.PR{URL: prURL, HeadSHA: expectedHead}
	snapshot, err := host.GetFeedback(cmd.Context(), pr)
	if err != nil {
		result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, ProviderSupported: true, StateReadable: false})
		result.Reason = err.Error()
		return emitReadiness(cmd, prURL, readinessPhase, result)
	}
	if strings.TrimSpace(snapshot.PRAuthor) == "" || strings.TrimSpace(snapshot.HeadSHA) == "" {
		result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, CurrentHead: snapshot.HeadSHA, ProviderSupported: true, StateReadable: false})
		result.Reason = "GitHub feedback state omitted pull-request author or head"
		return emitReadiness(cmd, prURL, readinessPhase, result)
	}
	checks, err := host.GetChecks(cmd.Context(), pr)
	if err != nil {
		result := scm.EvaluatePRReadiness(scm.ReadinessInput{Phase: readinessPhase, ExpectedHead: expectedHead, ProviderSupported: true, StateReadable: false})
		result.Reason = err.Error()
		return emitReadiness(cmd, prURL, readinessPhase, result)
	}
	ciReady := len(checks) > 0
	if len(checks) == 0 {
		// An empty check rollup is acceptable only when the repository's
		// trusted default-branch config explicitly declares no_ci: true.
		// Never read the checked-out feature branch for this decision.
		ciReady = trustedNoCI(envRepoForReadiness(prURL))
	}
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
		if item.Resolved || !policy.InScope(item) || markerAddresses(item, snapshot.Items, snapshot.PRAuthor, snapshot.HeadSHA) {
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

func envRepoForReadiness(prURL string) *db.Repo {
	env, err := openAxiEnv(false)
	if err != nil {
		return nil
	}
	defer env.close()
	return env.repo
}

func trustedNoCI(repo *db.Repo) bool {
	if repo == nil || strings.TrimSpace(repo.WorkingPath) == "" || strings.TrimSpace(repo.DefaultBranch) == "" {
		return false
	}
	ref := repo.DefaultBranch + ":.no-mistakes.yaml"
	data, err := exec.Command("git", "-C", repo.WorkingPath, "show", ref).Output()
	if err != nil {
		return false
	}
	cfg, err := config.LoadRepoFromBytes(data)
	return err == nil && cfg != nil && cfg.NoCI
}

func markerAddresses(item scm.FeedbackItem, all []scm.FeedbackItem, prAuthor, head string) bool {
	for _, candidate := range all {
		// A disposition is a separate PR-author reply bound to the source ID,
		// exact validated head, and explicit disposition. A generic later
		// comment, or a marker authored by someone else, never counts.
		if !strings.EqualFold(strings.TrimSpace(candidate.Author), strings.TrimSpace(prAuthor)) {
			continue
		}
		markerID, markerHead, disposition, ok := scm.ParseFeedbackDispositionMarker(candidate.Body)
		if ok && strings.TrimSpace(markerID) == strings.TrimSpace(item.ID) && strings.EqualFold(markerHead, strings.TrimSpace(head)) && strings.TrimSpace(disposition) != "" {
			return true
		}
	}
	return false
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
		proofAccepted := false
		proofArtifacts := []types.TestArtifact(nil)
		for _, step := range steps {
			if step.StepName == types.StepProof && step.Status == types.StepStatusCompleted && step.FindingsJSON != nil {
				findings, parseErr := types.ParseFindingsJSON(*step.FindingsJSON)
				if parseErr != nil || strings.TrimSpace(findings.Summary) == "" || len(findings.Tested) == 0 || strings.TrimSpace(findings.TestingSummary) == "" || len(findings.Artifacts) == 0 {
					return false
				}
				proofArtifacts = findings.Artifacts
			}
			if step.StepName == types.StepProofReview && step.Status == types.StepStatusCompleted {
				if step.FindingsJSON == nil {
					return false
				}
				var wire map[string]json.RawMessage
				if json.Unmarshal([]byte(*step.FindingsJSON), &wire) != nil || strings.TrimSpace(string(wire["summary"])) == "" || wire["findings"] == nil {
					return false
				}
				proofAccepted = true
			}
		}
		if proofAccepted && len(proofArtifacts) > 0 {
			pathsCfg, pathsErr := paths.New()
			if pathsErr != nil {
				return false
			}
			worktreeRoot, rootErr := filepath.EvalSymlinks(run.WorktreePath())
			if rootErr != nil {
				return false
			}
			evidenceRoot := filepath.Join(pathsCfg.EvidenceDir(), run.ID)
			evidenceRoot, rootErr = filepath.EvalSymlinks(evidenceRoot)
			if rootErr != nil {
				return false
			}
			for _, artifact := range proofArtifacts {
				path := strings.TrimSpace(artifact.Path)
				if path == "" {
					return false
				}
				if !filepath.IsAbs(path) {
					worktree := run.WorktreePath()
					if worktree == "" {
						return false
					}
					path = filepath.Join(worktree, filepath.Clean(path))
				}
				path, rootErr = filepath.EvalSymlinks(path)
				if rootErr != nil || (!pathWithinReadiness(path, worktreeRoot) && !pathWithinReadiness(path, evidenceRoot)) {
					return false
				}
				info, statErr := os.Stat(path)
				if statErr != nil || !info.Mode().IsRegular() || (run.CreatedAt > 0 && info.ModTime().Unix() < run.CreatedAt) || artifact.Bytes != info.Size() {
					return false
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					return false
				}
				sum := sha256.Sum256(data)
				if !strings.EqualFold(strings.TrimSpace(artifact.SHA256), hex.EncodeToString(sum[:])) {
					return false
				}
			}
			return true
		}
	}
	return false
}

func pathWithinReadiness(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func emitReadiness(cmd *cobra.Command, prURL string, phase scm.ReadinessPhase, result scm.ReadinessResult) error {
	fields := []toon.Field{{Key: "pr", Value: prURL}, {Key: "phase", Value: string(phase)}, {Key: "ready", Value: result.Ready}, {Key: "head", Value: result.Head}, {Key: "proof_review", Value: result.ProofReview}, {Key: "ci", Value: result.CI}, {Key: "review_decision", Value: result.ReviewDecision}, {Key: "unresolved_item_ids", Value: result.UnresolvedIDs}, {Key: "unresolved_item_urls", Value: result.UnresolvedURLs}, {Key: "unknown", Value: result.Unknown}, {Key: "reason", Value: result.Reason}}
	emitDoc(cmd, fields...)
	if result.Ready {
		return nil
	}
	return &exitError{code: 1, err: fmt.Errorf("PR is not ready: %s", result.Reason)}
}
