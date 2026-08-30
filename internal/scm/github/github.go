// Package github implements scm.Host backed by the gh CLI.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to GitHub through the gh CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's GitHub hostname; scopes the auth check
	repo         string // "owner/name" slug for --repo; empty when unknown
	forkOwner    string // fork owner for cross-repository PR heads
}

// New builds a Host. cliAvailable reports whether the gh binary is
// resolvable on the caller's PATH (possibly overridden by env). host is the
// repo's GitHub hostname; when set the availability check is scoped to it via
// --hostname so a stale credential for an unrelated configured gh host cannot
// make this repo look unauthenticated. repo is the "owner/name" slug; when set
// it is passed via --repo to every PR/run command so they resolve the right
// repository regardless of the process working directory. The daemon runs from
// a fixed, non-repo working dir, so without this gh cannot infer the repo (or
// branch) and fails on every poll. host is optional; empty reproduces the
// legacy unscoped auth-check behavior.
func New(cmd CmdFactory, cliAvailable func() bool, host, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		repo:         strings.TrimSpace(repo),
	}
}

// NewWithFork builds a Host that opens PRs on repo using forkRepo as the head
// repository owner. forkRepo is an "owner/name" slug; only the owner is needed
// because gh pr create expects --head <owner>:<branch>. host is optional; see
// New for its role in scoping the auth check.
func NewWithFork(cmd CmdFactory, cliAvailable func() bool, host, repo, forkRepo string) *Host {
	h := New(cmd, cliAvailable, host, repo)
	h.forkOwner = repoOwner(forkRepo)
	return h
}

// RepoSlug extracts the "owner/name" identifier from a GitHub remote or PR
// URL. Longer paths such as PR links are reduced to their leading two segments.
func RepoSlug(remoteURL string) string {
	parts := strings.Split(scm.RepoPath(remoteURL), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// HostPrefixedSlug returns "host/owner/name" for GitHub Enterprise Server
// instances and plain "owner/name" for github.com. This is the format that
// the gh CLI's --repo flag requires for GHE.
func HostPrefixedSlug(remoteURL string) string {
	return HostPrefixedSlugForHost(remoteURL, scm.ExtractHost(remoteURL))
}

// HostPrefixedSlugForHost is HostPrefixedSlug using an already-resolved host.
// This lets callers honor SSH HostName aliases without rewriting the remote.
func HostPrefixedSlugForHost(remoteURL, host string) string {
	slug := RepoSlug(remoteURL)
	if slug == "" {
		return ""
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || strings.EqualFold(host, "github.com") {
		return slug
	}
	return host + "/" + slug
}

// repoArgs returns the --repo flag pair when the slug is known, so gh commands
// resolve the right repository regardless of the process working directory.
func (h *Host) repoArgs() []string {
	if h.repo == "" {
		return nil
	}
	return []string{"--repo", h.repo}
}

// prSelector returns the explicit gh PR selector for pr, preferring the numeric
// PR number and falling back to the canonical PR URL; both name the exact pull
// request to gh regardless of the process working directory. It fails closed
// when neither is known: an empty positional makes `gh pr <verb>` fall back to
// resolving the current branch of the cwd, and the daemon runs from a detached
// bare gate repo whose HEAD is the default branch (main), so an inferred
// selector silently targets the wrong PR (or none — "no pull requests found for
// branch main") instead of the feature PR the pipeline already knows.
func prSelector(pr *scm.PR) (string, error) {
	if pr != nil {
		if n := strings.TrimSpace(pr.Number); n != "" {
			return n, nil
		}
		if u := strings.TrimSpace(pr.URL); u != "" {
			return u, nil
		}
	}
	return "", errors.New("no PR number or URL known; refusing to run gh with a cwd-inferred branch")
}

func (h *Host) headRef(branch string) string {
	if h.forkOwner == "" {
		return branch
	}
	return h.forkOwner + ":" + branch
}

func repoOwner(slug string) string {
	owner, _, ok := strings.Cut(strings.TrimSpace(slug), "/")
	if !ok {
		return ""
	}
	return strings.TrimSpace(owner)
}

func (h *Host) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true, ReviewComments: true}
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("gh CLI is not installed")
	}
	// Scope the auth check to this repo's host. Unscoped `gh auth status`
	// checks every authenticated account and exits non-zero if ANY of them has
	// a stale/expired token, even when this repo's own host is fully
	// authenticated. Passing --hostname keeps an unrelated bad credential from
	// poisoning availability for this repo. When the host is unknown we fall
	// back to the unscoped check (fail-safe: same behavior as before).
	authArgs := []string{"auth", "status"}
	if h.host != "" {
		authArgs = append(authArgs, "--hostname", h.host)
	}
	if err := h.cmd(ctx, "gh", authArgs...).Run(); err != nil {
		return errors.New("gh CLI is not authenticated")
	}
	return nil
}

func parsePullRequestURL(raw, expectedHost, expectedRepo string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return 0, errors.New("expected absolute GitHub pull request URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return 0, errors.New("expected HTTP GitHub pull request URL")
	}
	if expectedHost != "" && !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return 0, fmt.Errorf("URL host %q does not match GitHub host %q", parsed.Hostname(), expectedHost)
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 4 || segments[2] != "pull" {
		return 0, errors.New("expected GitHub /owner/repo/pull/number URL")
	}
	for _, segment := range segments[:2] {
		if segment == "" || segment == "." || segment == ".." {
			return 0, errors.New("expected unambiguous GitHub owner/repository path")
		}
	}
	actualRepo := segments[0] + "/" + segments[1]
	expectedRepo = strings.Trim(strings.TrimSpace(expectedRepo), "/")
	if expectedRepo != "" && !strings.EqualFold(actualRepo, expectedRepo) {
		return 0, fmt.Errorf("URL repository %q does not match GitHub repository %q", actualRepo, expectedRepo)
	}
	number, err := strconv.Atoi(segments[3])
	if err != nil || number <= 0 {
		return 0, errors.New("expected positive GitHub pull request number")
	}
	escapedSegments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(escapedSegments) != len(segments) || escapedSegments[len(escapedSegments)-1] != strconv.Itoa(number) {
		return 0, errors.New("expected canonical GitHub pull request number path")
	}
	if parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(trimmed, "#") {
		return 0, errors.New("expected GitHub pull request URL without query or fragment")
	}
	return number, nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"pr", "list", "--head", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--base", base)
	}
	args = append(args, h.repoArgs()...)
	jsonFields := "number,url,baseRefName"
	if h.forkOwner != "" {
		jsonFields = "number,url,baseRefName,headRefName,headRepositoryOwner"
	}
	args = append(args, "--state", "open", "--json", jsonFields)
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var prs []struct {
		Number              int    `json:"number"`
		URL                 string `json:"url"`
		BaseRefName         string `json:"baseRefName"`
		HeadRefName         string `json:"headRefName"`
		HeadRepositoryOwner *struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list JSON: %w", err)
	}
	if prs == nil {
		return nil, errors.New("parse gh pr list JSON: expected array")
	}
	if len(prs) == 0 {
		return nil, nil
	}
	prNumbers := make([]string, len(prs))
	for i, candidate := range prs {
		if candidate.Number <= 0 {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing positive PR number", i)
		}
		url := strings.TrimSpace(candidate.URL)
		if url == "" {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing PR URL", i)
		}
		number, err := parsePullRequestURL(url, h.host, h.repoSlug())
		if err != nil {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d invalid PR URL: %w", i, err)
		}
		if candidate.Number != number {
			return nil, fmt.Errorf("parse gh pr list JSON: entry %d PR number %d does not match URL number %d", i, candidate.Number, number)
		}
		prNumbers[i] = strconv.Itoa(candidate.Number)
		if h.forkOwner != "" {
			if strings.TrimSpace(candidate.HeadRefName) == "" {
				return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing headRefName", i)
			}
			if candidate.HeadRepositoryOwner == nil || strings.TrimSpace(candidate.HeadRepositoryOwner.Login) == "" {
				return nil, fmt.Errorf("parse gh pr list JSON: entry %d missing headRepositoryOwner login", i)
			}
		}
	}
	for i, candidate := range prs {
		if !h.matchesHead(candidate.HeadRefName, candidate.HeadRepositoryOwner, branch) {
			continue
		}
		pr := &scm.PR{
			Number:     prNumbers[i],
			URL:        strings.TrimSpace(candidate.URL),
			BaseBranch: strings.TrimSpace(candidate.BaseRefName),
		}
		return pr, nil
	}
	return nil, nil
}

func (h *Host) matchesHead(headRefName string, owner *struct {
	Login string `json:"login"`
}, branch string) bool {
	if h.forkOwner == "" {
		return true
	}
	if strings.TrimSpace(headRefName) != "" && headRefName != branch {
		return false
	}
	if owner == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(owner.Login), h.forkOwner)
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := append([]string{"pr", "create",
		"--head", h.headRef(branch),
		"--base", base,
	}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	url := strings.TrimSpace(string(out))
	pr := &scm.PR{URL: url}
	if num, nerr := scm.ExtractPRNumber(url); nerr == nil {
		pr.Number = num
	}
	return pr, nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	args := append([]string{"pr", "edit", selector}, h.repoArgs()...)
	args = append(args, "--title", content.Title, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(content.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh pr edit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "state", "--jq", ".state")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return normalizePRState(strings.TrimSpace(string(out))), nil
}

func (h *Host) GetPRBaseBranch(ctx context.Context, pr *scm.PR) (string, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "baseRefName", "--jq", ".baseRefName")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view base branch: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	headSHA := ""
	if strings.TrimSpace(pr.HeadSHA) != "" {
		headSHA, err = h.getPRHeadSHA(ctx, selector)
		if err != nil {
			return nil, err
		}
		pr.HeadSHA = headSHA
	}
	var checks []scm.Check
	if headSHA != "" {
		checks, err = h.getCommitChecks(ctx, headSHA)
	} else {
		checks, err = h.getPRChecks(ctx, selector)
	}
	if err != nil {
		return nil, err
	}
	if headSHA != "" {
		runs, err := h.getWorkflowRunChecks(ctx, headSHA)
		if err != nil {
			return nil, err
		}
		checks = h.appendUnrepresentedWorkflowRuns(checks, runs)
		checks = h.collapseLatestByName(checks)
		currentHeadSHA, err := h.getPRHeadSHA(ctx, selector)
		if err != nil {
			return nil, err
		}
		if currentHeadSHA != headSHA {
			return nil, fmt.Errorf("PR head changed during check discovery from %s to %s", headSHA, currentHeadSHA)
		}
	}
	return checks, nil
}

func (h *Host) getPRChecks(ctx context.Context, selector string) ([]scm.Check, error) {
	args := append([]string{"pr", "checks", selector}, h.repoArgs()...)
	args = append(args, "--json", "name,state,bucket,completedAt,link")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no checks reported") {
			out = []byte("[]")
		} else {
			return nil, fmt.Errorf("gh pr checks: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	var raw []struct {
		Name        string `json:"name"`
		State       string `json:"state"`
		Bucket      string `json:"bucket"`
		CompletedAt string `json:"completedAt"`
		Link        string `json:"link"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse CI checks: %w", err)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, r := range raw {
		var completedAt time.Time
		if r.CompletedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, r.CompletedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		checks = append(checks, scm.Check{
			Name:        r.Name,
			Bucket:      normalizeCheckBucket(r.Bucket, r.State),
			State:       strings.ToUpper(strings.TrimSpace(r.State)),
			CompletedAt: completedAt,
			Link:        strings.TrimSpace(r.Link),
		})
	}
	return checks, nil
}

const commitChecksQuery = `query($owner:String!,$name:String!,$oid:String!,$cursor:String){repository(owner:$owner,name:$name){object(expression:$oid){... on Commit{statusCheckRollup{contexts(first:100,after:$cursor){nodes{__typename ... on CheckRun{name status conclusion completedAt startedAt detailsUrl} ... on StatusContext{context state targetUrl}} pageInfo{hasNextPage endCursor}}}}}}}`

const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){nodes{id isResolved comments(first:100){nodes{databaseId body path line url createdAt author{login}} pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}}}}}`

func (h *Host) getCommitChecks(ctx context.Context, headSHA string) ([]scm.Check, error) {
	repo := h.repoSlug()
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("resolve GitHub repository for commit checks: invalid repository %q", repo)
	}
	var checks []scm.Check
	cursor := ""
	for {
		args := []string{"api"}
		if h.host != "" {
			args = append(args, "--hostname", h.host)
		}
		args = append(args, "graphql", "-f", "query="+commitChecksQuery,
			"-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "oid="+strings.TrimSpace(headSHA))
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("gh api checks for head commit: %s: %w", strings.TrimSpace(string(out)), err)
		}
		var response struct {
			Data struct {
				Repository *struct {
					Object *struct {
						Rollup *struct {
							Contexts struct {
								Nodes []struct {
									Type        string `json:"__typename"`
									Name        string `json:"name"`
									Status      string `json:"status"`
									Conclusion  string `json:"conclusion"`
									CompletedAt string `json:"completedAt"`
									StartedAt   string `json:"startedAt"`
									DetailsURL  string `json:"detailsUrl"`
									Context     string `json:"context"`
									State       string `json:"state"`
									TargetURL   string `json:"targetUrl"`
								} `json:"nodes"`
								PageInfo struct {
									HasNextPage bool   `json:"hasNextPage"`
									EndCursor   string `json:"endCursor"`
								} `json:"pageInfo"`
							} `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"object"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("parse checks for head commit: %w", err)
		}
		if response.Data.Repository == nil || response.Data.Repository.Object == nil {
			return nil, errors.New("head commit check discovery returned no commit")
		}
		if response.Data.Repository.Object.Rollup == nil {
			return checks, nil
		}
		contexts := response.Data.Repository.Object.Rollup.Contexts
		for _, node := range contexts.Nodes {
			check := scm.Check{}
			switch node.Type {
			case "CheckRun":
				check.Kind = scm.CheckKindRun
				check.Name = strings.TrimSpace(node.Name)
				check.State = strings.ToUpper(strings.TrimSpace(node.Conclusion))
				if check.State == "" {
					check.State = strings.ToUpper(strings.TrimSpace(node.Status))
				}
				check.Bucket = normalizeCheckBucket("", node.Conclusion)
				if check.Bucket == "" {
					check.Bucket = normalizeCheckBucket("", node.Status)
				}
				check.Link = strings.TrimSpace(node.DetailsURL)
				if parsed, parseErr := time.Parse(time.RFC3339, node.CompletedAt); parseErr == nil {
					check.CompletedAt = parsed
				}
				if parsed, parseErr := time.Parse(time.RFC3339, node.StartedAt); parseErr == nil {
					check.StartedAt = parsed
				}
			case "StatusContext":
				check.Kind = scm.CheckKindStatus
				check.Name = strings.TrimSpace(node.Context)
				check.State = strings.ToUpper(strings.TrimSpace(node.State))
				check.Bucket = normalizeCheckBucket("", node.State)
				check.Link = strings.TrimSpace(node.TargetURL)
			default:
				return nil, fmt.Errorf("head commit check discovery returned unsupported context type %q", node.Type)
			}
			if check.Name == "" || check.Bucket == "" {
				return nil, errors.New("head commit check discovery returned an incomplete context")
			}
			checks = append(checks, check)
		}
		if !contexts.PageInfo.HasNextPage {
			return checks, nil
		}
		if contexts.PageInfo.EndCursor == "" || contexts.PageInfo.EndCursor == cursor {
			return nil, errors.New("head commit check discovery returned an invalid page cursor")
		}
		cursor = contexts.PageInfo.EndCursor
	}
}

func (h *Host) repoSlug() string {
	repo := strings.TrimSpace(h.repo)
	if prefix := strings.TrimSpace(h.host) + "/"; h.host != "" && len(repo) > len(prefix) && strings.EqualFold(repo[:len(prefix)], prefix) {
		repo = repo[len(prefix):]
	}
	return repo
}

func (h *Host) appendUnrepresentedWorkflowRuns(checks, runs []scm.Check) []scm.Check {
	represented := make(map[string][]int, len(checks))
	for i, check := range checks {
		if runID := h.actionsRunID(check.Link); runID != "" {
			represented[runID] = append(represented[runID], i)
		}
	}
	for _, run := range runs {
		runID := h.actionsRunID(run.Link)
		if indices := represented[runID]; runID != "" && len(indices) > 0 {
			for _, i := range indices {
				if checks[i].StartedAt.IsZero() && !run.StartedAt.IsZero() {
					checks[i].StartedAt = run.StartedAt
				}
				checks[i].WorkflowID = run.WorkflowID
			}
			continue
		}
		checks = append(checks, run)
		if runID != "" {
			represented[runID] = []int{len(checks) - 1}
		}
	}
	return checks
}

// collapseLatestByName collapses orderable same-name reruns of one workflow
// to the most recently started one. Independent workflows and records whose
// provider metadata cannot establish an order remain visible. GitHub's raw
// commit statusCheckRollup returns every check run ever attached to the commit,
// including runs a later same-named run has
// already superseded - e.g. a CI monitor's auto-fix push re-triggers the
// same gate check, and the rollup keeps both the old FAILURE and the new
// SUCCESS forever. Without this collapse the superseded failure stays
// visible even after the later run at the same head turns green, which
// manufactures an unrecoverable auto-fix loop (see AGENTS.md "CI Monitor
// Lifecycle"). This restores the semantics `gh pr checks` already applies
// (collapse by startedAt) to the commit-rollup path, which never had it.
//
// Must run AFTER appendUnrepresentedWorkflowRuns, never before: that call
// dedupes by Actions run ID against the FULL uncollapsed rollup. Collapsing
// first would drop a superseded run's ID out of the "represented" set the
// union checks against, letting the union re-add the same stale run under
// its own workflow run name - resurrecting exactly the failure this is
// meant to hide.
func (h *Host) collapseLatestByName(checks []scm.Check) []scm.Check {
	collapsed := make([]scm.Check, 0, len(checks))
	for _, check := range checks {
		keep := true
		for i := 0; i < len(collapsed); {
			other := collapsed[i]
			if !h.sameCheckReplacementGroup(check, other) {
				i++
				continue
			}
			after, ordered := h.checkStartedAfter(check, other)
			if !ordered {
				i++
				continue
			}
			if !after {
				keep = false
				break
			}
			collapsed = append(collapsed[:i], collapsed[i+1:]...)
		}
		if keep {
			collapsed = append(collapsed, check)
		}
	}
	return collapsed
}

func (h *Host) sameCheckReplacementGroup(a, b scm.Check) bool {
	if a.Kind != scm.CheckKindRun || b.Kind != scm.CheckKindRun || a.Name != b.Name {
		return false
	}
	// Only distinct runs of the same known workflow establish rerun identity.
	// Missing workflow/run identities may be independent external checks, while
	// equal run identities may be independent same-name jobs within one run.
	// Collapsing either case could hide a failing requirement.
	aRunID := h.actionsRunID(a.Link)
	bRunID := h.actionsRunID(b.Link)
	return a.WorkflowID != 0 && a.WorkflowID == b.WorkflowID &&
		aRunID != "" && bRunID != "" && aRunID != bRunID
}

// checkStartedAfter reports whether a is newer and whether the available
// provider metadata establishes an order between the checks.
func (h *Host) checkStartedAfter(a, b scm.Check) (bool, bool) {
	if !a.StartedAt.IsZero() && !b.StartedAt.IsZero() && !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt), true
	}
	if aID, aErr := strconv.ParseUint(h.actionsRunID(a.Link), 10, 64); aErr == nil {
		if bID, bErr := strconv.ParseUint(h.actionsRunID(b.Link), 10, 64); bErr == nil && aID != bID {
			return aID > bID, true
		}
	}
	if a.StartedAt.IsZero() != b.StartedAt.IsZero() {
		return false, false
	}
	if !a.CompletedAt.IsZero() && !b.CompletedAt.IsZero() && !a.CompletedAt.Equal(b.CompletedAt) {
		return a.CompletedAt.After(b.CompletedAt), true
	}
	return false, false
}

func (h *Host) getPRHeadSHA(ctx context.Context, selector string) (string, error) {
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "headRefOid", "--jq", ".headRefOid")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view head commit: %w", err)
	}
	headSHA := strings.TrimSpace(string(out))
	if headSHA == "" {
		return "", errors.New("gh pr view returned an empty head commit")
	}
	return headSHA, nil
}

func (h *Host) getWorkflowRunChecks(ctx context.Context, headSHA string) ([]scm.Check, error) {
	repo := h.repoSlug()
	endpoint := "repos/{owner}/{repo}/actions/runs"
	if repo != "" {
		endpoint = "repos/" + repo + "/actions/runs"
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint,
		"-f", "head_sha="+strings.TrimSpace(headSHA),
		"-f", "per_page=100",
		"--paginate", "--slurp",
	)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api workflow runs for head commit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	type workflowRun struct {
		ID           int64  `json:"id"`
		WorkflowID   int64  `json:"workflow_id"`
		Name         string `json:"name"`
		DisplayName  string `json:"display_title"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		RunStartedAt string `json:"run_started_at"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
		HTMLURL      string `json:"html_url"`
	}
	var pages []struct {
		TotalCount   *int          `json:"total_count"`
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("parse workflow runs for head commit: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("workflow run discovery returned no pages")
	}
	var raw []workflowRun
	totalCount := -1
	runIDs := make(map[int64]struct{})
	for pageIndex, page := range pages {
		if page.TotalCount == nil || *page.TotalCount < 0 {
			return nil, fmt.Errorf("workflow run page %d has no valid total_count", pageIndex+1)
		}
		if totalCount == -1 {
			totalCount = *page.TotalCount
		} else if *page.TotalCount != totalCount {
			return nil, fmt.Errorf("workflow run page %d total_count is %d, want %d", pageIndex+1, *page.TotalCount, totalCount)
		}
		for _, run := range page.WorkflowRuns {
			if run.ID == 0 {
				return nil, fmt.Errorf("workflow run page %d contains a run without an id", pageIndex+1)
			}
			if _, exists := runIDs[run.ID]; exists {
				return nil, fmt.Errorf("workflow run id %d appears more than once", run.ID)
			}
			runIDs[run.ID] = struct{}{}
			raw = append(raw, run)
		}
	}
	if len(runIDs) != totalCount {
		return nil, fmt.Errorf("workflow run discovery returned %d unique runs, want %d", len(runIDs), totalCount)
	}
	checks := make([]scm.Check, 0, len(raw))
	for _, run := range raw {
		name := strings.TrimSpace(run.Name)
		if name == "" {
			name = strings.TrimSpace(run.DisplayName)
		}
		if name == "" {
			name = "GitHub Actions workflow"
		}
		var startedAt time.Time
		for _, timestamp := range []string{run.RunStartedAt, run.CreatedAt} {
			if parsed, parseErr := time.Parse(time.RFC3339, timestamp); parseErr == nil {
				startedAt = parsed
				break
			}
		}
		var completedAt time.Time
		if run.UpdatedAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339, run.UpdatedAt); parseErr == nil {
				completedAt = parsed
			}
		}
		bucket := normalizeCheckBucket("", run.Conclusion)
		if bucket == "" {
			bucket = normalizeCheckBucket("", run.Status)
		}
		if bucket == "" {
			// An unrecognized or incomplete run state must not certify the
			// commit as green. Keep monitoring until GitHub reports a terminal
			// state that can be classified.
			bucket = scm.CheckBucketPending
		}
		state := strings.ToUpper(strings.TrimSpace(run.Conclusion))
		if state == "" {
			state = strings.ToUpper(strings.TrimSpace(run.Status))
		}
		link := strings.TrimSpace(run.HTMLURL)
		if link == "" {
			host := strings.TrimSpace(h.host)
			if host == "" {
				host = "github.com"
			}
			link = fmt.Sprintf("https://%s/%s/actions/runs/%d", host, repo, run.ID)
		}
		checks = append(checks, scm.Check{Name: name, Bucket: bucket, Kind: scm.CheckKindRun, State: state, CompletedAt: completedAt, StartedAt: startedAt, WorkflowID: run.WorkflowID, Link: link})
	}
	return checks, nil
}

// RerunCheck re-runs the Actions work behind check for the same commit, so a
// check the provider cancelled rather than failed can be retried without a new
// push. The job is identified from the check's details link, which is the only
// run/job identity `gh pr checks` reports: a link naming a job re-runs just that
// job (and its dependencies), and a cancelled check naming only a run re-runs
// the whole run. Anything else - a third-party status pointing at an external
// dashboard, or a run path this backend cannot read - names no re-runnable work,
// and the error says so rather than falling back to a wider rerun.
func (h *Host) RerunCheck(ctx context.Context, _ *scm.PR, check scm.Check) error {
	rerunArgs, ok := h.rerunTargetArgs(check)
	if !ok {
		return fmt.Errorf("check %q has no GitHub Actions job to re-run", check.Name)
	}
	args := append([]string{"run", "rerun"}, rerunArgs...)
	args = append(args, h.repoArgs()...)
	cmd := h.cmd(ctx, "gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh run rerun: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// rerunTargetArgs turns a check into the `gh run rerun` arguments that re-run
// exactly that work, or reports that the link names nothing this backend can
// re-run.
func (h *Host) rerunTargetArgs(check scm.Check) ([]string, bool) {
	runID, jobID, ok := h.actionsRerunTarget(check.Link)
	switch {
	case !ok:
		return nil, false
	case jobID != "":
		return []string{"--job", jobID}, true
	case strings.EqualFold(strings.TrimSpace(check.State), "CANCELLED"):
		return []string{runID}, true
	default:
		return []string{runID, "--failed"}, true
	}
}

func (h *Host) actionsRunID(link string) string {
	segments, ok := h.actionsRunSegments(link)
	if !ok {
		return ""
	}
	return segments[0]
}

func (h *Host) actionsRunSegments(link string) ([]string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return nil, false
	}
	host := strings.TrimSpace(h.host)
	if host == "" {
		host = "github.com"
	}
	if !strings.EqualFold(parsed.Hostname(), host) {
		return nil, false
	}
	repo := h.repoSlug()
	if repo == "" {
		return nil, false
	}
	runsPrefix := "/" + strings.Trim(repo, "/") + "/actions/runs/"
	if len(parsed.Path) <= len(runsPrefix) || !strings.EqualFold(parsed.Path[:len(runsPrefix)], runsPrefix) {
		return nil, false
	}
	segments := strings.Split(strings.Trim(parsed.Path[len(runsPrefix):], "/"), "/")
	if len(segments) == 0 || !isNumericID(segments[0]) {
		return nil, false
	}
	return segments, true
}

// actionsRerunTarget resolves the Actions run, and where possible the exact job,
// that a check's details URL names. It parses the URL and reads only its path:
// a real details URL routinely carries a query (?check_suite_focus=true) or a
// step fragment (#step:4:12), and neither belongs to the job identity.
//
// Only ".../actions/runs/<run-id>/job/<id>" yields a job id, because that `id`
// is the job's databaseId, which is what `gh run rerun --job` requires. Two
// shapes are deliberately rejected rather than downgraded to the whole run:
// the browser's ".../runs/<run-id>/jobs/<n>" form, whose number is a per-run
// display index the API answers with 404, and any other unrecognized path under
// a run. Re-running a run can restart more than one job, so widening one
// check's rerun on an unparsable link is a blast radius this policy must not
// take.
func (h *Host) actionsRerunTarget(link string) (runID, jobID string, ok bool) {
	segments, ok := h.actionsRunSegments(link)
	if !ok {
		return "", "", false
	}
	runID = segments[0]
	switch {
	case len(segments) == 1:
		return runID, "", true
	case len(segments) == 3 && segments[1] == "job" && isNumericID(segments[2]):
		return runID, segments[2], true
	default:
		return "", "", false
	}
}

// PreRunFailures reports which of the given failed checks GitHub Actions failed
// during the job's setup phase - before any repository step ran. Actions
// resolves and downloads every action a job uses inside "Set up job", so an
// action-download outage ("Failed to resolve action download info", HTTP 503)
// fails that step and the job never executes a repository step. It reads the
// job's own step-level conclusions, never log text, and fails closed: a check
// whose job it cannot resolve or read is simply not flagged, so it stays a
// genuine failure. The result is positional (parallel to checks), so a
// same-named genuine failure never inherits another check's infrastructure flag.
func (h *Host) PreRunFailures(ctx context.Context, checks []scm.Check) ([]bool, error) {
	result := make([]bool, len(checks))
	// Cache each run's jobs so several checks from one run cost one API call.
	runJobs := map[string][]githubRunJob{}
	for i, check := range checks {
		runID, jobID, ok := h.actionsRerunTarget(check.Link)
		if !ok {
			continue
		}
		jobs, seen := runJobs[runID]
		if !seen {
			jobs = h.fetchRunJobs(ctx, runID)
			runJobs[runID] = jobs
		}
		job, found := matchRunJob(jobs, jobID, check.Name)
		if found && jobFailedAtSetup(job) {
			result[i] = true
		}
	}
	return result, nil
}

// fetchRunJobs reads a run's jobs (with their steps) from Actions. A run it
// cannot read yields no jobs, so every check on it fails closed to a genuine
// failure rather than being guessed as infrastructure.
func (h *Host) fetchRunJobs(ctx context.Context, runID string) []githubRunJob {
	viewArgs := append([]string{"run", "view", runID}, h.repoArgs()...)
	viewArgs = append(viewArgs, "--json", "jobs")
	out, err := h.cmd(ctx, "gh", viewArgs...).Output()
	if err != nil {
		return nil
	}
	var payload githubRunView
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	return payload.Jobs
}

// matchRunJob finds the job a check names: by databaseId when the check's link
// carried one, otherwise by job name. A re-run can renumber jobs, so the name
// fallback keeps a check matchable when its link named only the run.
func matchRunJob(jobs []githubRunJob, jobID, checkName string) (githubRunJob, bool) {
	if jobID != "" {
		for _, job := range jobs {
			if strconv.Itoa(job.DatabaseID) == jobID {
				return job, true
			}
		}
	}
	for _, job := range jobs {
		if normalizeRunName(job.Name) == normalizeRunName(checkName) {
			return job, true
		}
	}
	return githubRunJob{}, false
}

// jobFailedAtSetup reports whether a failed job failed in its setup step, before
// any repository step ran. It requires the job itself to be failed and its setup
// step ("Set up job", always step 1) to carry a failure conclusion; a job whose
// setup succeeded and failed a later step is a real failure and is never matched.
func jobFailedAtSetup(job githubRunJob) bool {
	if !isFailedJob(job) {
		return false
	}
	for _, step := range job.Steps {
		if step.Number == 1 || strings.EqualFold(strings.TrimSpace(step.Name), "Set up job") {
			return strings.EqualFold(strings.TrimSpace(step.Conclusion), "failure")
		}
	}
	return false
}

func isNumericID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	selector, err := prSelector(pr)
	if err != nil {
		return "", err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "mergeable", "--jq", ".mergeable")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view mergeable: %w", err)
	}
	return normalizeMergeableState(strings.TrimSpace(string(out))), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, _ *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	targets := make(map[string]struct{}, len(failingNames))
	for _, name := range failingNames {
		name = normalizeRunName(name)
		if name != "" {
			targets[name] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	args := []string{"run", "list", "--branch", branch}
	if strings.TrimSpace(headSHA) != "" {
		args = append(args, "--commit", strings.TrimSpace(headSHA))
	}
	args = append(args, h.repoArgs()...)
	args = append(args,
		"--status", "failure",
		"--limit", "20",
		"--json", "databaseId,headSha,name,displayTitle,workflowName",
	)
	listCmd := h.cmd(ctx, "gh", args...)
	listOut, err := listCmd.Output()
	if err != nil {
		return "", nil
	}
	var runs []githubRun
	if err := json.Unmarshal(listOut, &runs); err != nil {
		return "", nil
	}
	for _, run := range runs {
		if !runMatchesTargets(ctx, h, run, targets) {
			continue
		}
		viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
		viewArgs = append(viewArgs, "--log-failed")
		viewCmd := h.cmd(ctx, "gh", viewArgs...)
		out, err := viewCmd.Output()
		if err != nil {
			continue
		}
		logs := strings.TrimSpace(string(out))
		if logs != "" {
			return logs, nil
		}
	}
	return "", nil
}

type githubRun struct {
	DatabaseID   int    `json:"databaseId"`
	HeadSHA      string `json:"headSha"`
	Name         string `json:"name"`
	DisplayTitle string `json:"displayTitle"`
	WorkflowName string `json:"workflowName"`
}

type githubRunView struct {
	Jobs []githubRunJob `json:"jobs"`
}

type githubRunJob struct {
	DatabaseID int             `json:"databaseId"`
	Name       string          `json:"name"`
	Conclusion string          `json:"conclusion"`
	Status     string          `json:"status"`
	Steps      []githubJobStep `json:"steps"`
}

type githubJobStep struct {
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func runMatchesTargets(ctx context.Context, h *Host, run githubRun, targets map[string]struct{}) bool {
	for _, candidate := range []string{run.Name, run.DisplayTitle, run.WorkflowName} {
		if _, ok := targets[normalizeRunName(candidate)]; ok {
			return true
		}
	}
	if run.DatabaseID == 0 {
		return false
	}
	viewArgs := append([]string{"run", "view", fmt.Sprintf("%d", run.DatabaseID)}, h.repoArgs()...)
	viewArgs = append(viewArgs, "--json", "jobs")
	viewCmd := h.cmd(ctx, "gh", viewArgs...)
	out, err := viewCmd.Output()
	if err != nil {
		return false
	}
	var payload githubRunView
	if err := json.Unmarshal(out, &payload); err != nil {
		return false
	}
	for _, job := range payload.Jobs {
		if !isFailedJob(job) {
			continue
		}
		if _, ok := targets[normalizeRunName(job.Name)]; ok {
			return true
		}
	}
	return false
}

func isFailedJob(job githubRunJob) bool {
	state := strings.ToUpper(strings.TrimSpace(job.Conclusion))
	if state == "" {
		state = strings.ToUpper(strings.TrimSpace(job.Status))
	}
	switch state {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return true
	default:
		return false
	}
}

func normalizeRunName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OPEN":
		return scm.PRStateOpen
	case "MERGED":
		return scm.PRStateMerged
	case "CLOSED":
		return scm.PRStateClosed
	default:
		return scm.PRState(raw)
	}
}

func normalizeMergeableState(raw string) scm.MergeableState {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MERGEABLE":
		return scm.MergeableOK
	case "CONFLICTING":
		return scm.MergeableConflict
	case "UNKNOWN", "":
		return scm.MergeablePending
	default:
		return scm.MergeableState(raw)
	}
}

func normalizeCheckBucket(bucket, state string) scm.CheckBucket {
	if normalized := scm.CheckBucket(strings.TrimSpace(bucket)); normalized != "" {
		return normalized
	}

	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return scm.CheckBucketPass
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return scm.CheckBucketFail
	case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "EXPECTED":
		return scm.CheckBucketPending
	case "CANCELLED":
		return scm.CheckBucketCancel
	case "SKIPPED", "NEUTRAL", "STALE":
		return scm.CheckBucketSkip
	default:
		return ""
	}
}

// GetReviewComments implements scm.ReviewCommentsHost.
func (h *Host) GetReviewComments(ctx context.Context, pr *scm.PR) ([]scm.ReviewComment, error) {
	if pr == nil {
		return nil, errors.New("pr is nil")
	}
	repo := h.repoSlug()
	if repo == "" && pr.URL != "" {
		repo = RepoSlug(pr.URL)
	}
	if repo == "" {
		return nil, errors.New("cannot determine repository for PR review comments")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("resolve GitHub repository for PR review comments: invalid repository %q", repo)
	}
	prNum := strings.TrimSpace(pr.Number)
	if prNum == "" {
		number, parseErr := parsePullRequestURL(pr.URL, h.host, repo)
		if parseErr != nil {
			return nil, parseErr
		}
		prNum = strconv.Itoa(number)
	}
	number, err := strconv.Atoi(prNum)
	if err != nil || number <= 0 {
		return nil, errors.New("expected positive GitHub pull request number")
	}

	var comments []scm.ReviewComment
	seenComments := make(map[string]struct{})
	cursor := ""
	for {
		args := []string{"api"}
		if h.host != "" {
			args = append(args, "--hostname", h.host)
		}
		args = append(args, "graphql", "-f", "query="+reviewThreadsQuery,
			"-F", "owner="+parts[0], "-F", "name="+parts[1], "-F", "number="+strconv.Itoa(number))
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		out, commandErr := h.cmd(ctx, "gh", args...).CombinedOutput()
		if commandErr != nil {
			return nil, fmt.Errorf("gh api PR review comments: %s: %w", strings.TrimSpace(string(out)), commandErr)
		}
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads struct {
							Nodes []struct {
								ID         string `json:"id"`
								IsResolved bool   `json:"isResolved"`
								Comments   struct {
									Nodes []struct {
										ID        int64     `json:"databaseId"`
										Body      string    `json:"body"`
										Path      string    `json:"path"`
										Line      *int      `json:"line"`
										URL       string    `json:"url"`
										CreatedAt time.Time `json:"createdAt"`
										Author    *struct {
											Login string `json:"login"`
										} `json:"author"`
									} `json:"nodes"`
									PageInfo struct {
										HasNextPage bool   `json:"hasNextPage"`
										EndCursor   string `json:"endCursor"`
									} `json:"pageInfo"`
								} `json:"comments"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("decode PR review comments JSON: %w", err)
		}
		if len(response.Errors) > 0 {
			return nil, fmt.Errorf("gh api PR review comments: %s", response.Errors[0].Message)
		}
		if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
			return nil, errors.New("PR review comments response did not contain the pull request")
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			if thread.IsResolved {
				continue
			}
			for _, raw := range thread.Comments.Nodes {
				if raw.Author == nil || !isSupportedReviewBot(raw.Author.Login) {
					continue
				}
				id := strconv.FormatInt(raw.ID, 10)
				if _, seen := seenComments[id]; seen {
					continue
				}
				seenComments[id] = struct{}{}
				line := 0
				if raw.Line != nil {
					line = *raw.Line
				}
				comments = append(comments, scm.ReviewComment{
					ID:        id,
					Author:    raw.Author.Login,
					Path:      raw.Path,
					Line:      line,
					Body:      raw.Body,
					CreatedAt: raw.CreatedAt,
					URL:       raw.URL,
				})
			}
			commentCursor := thread.Comments.PageInfo.EndCursor
			commentsHasNext := thread.Comments.PageInfo.HasNextPage
			for commentsHasNext {
				if thread.ID == "" || commentCursor == "" {
					return nil, errors.New("PR review comments response returned an invalid nested comment cursor")
				}
				more, next, hasNext, fetchErr := h.getFeedbackThreadComments(ctx, thread.ID, commentCursor)
				if fetchErr != nil {
					return nil, fetchErr
				}
				for _, raw := range more {
					if raw.Author == nil || !isSupportedReviewBot(raw.Author.Login) {
						continue
					}
					id := strconv.FormatInt(raw.ID, 10)
					if _, seen := seenComments[id]; seen {
						continue
					}
					seenComments[id] = struct{}{}
					line := 0
					if raw.Line != nil {
						line = *raw.Line
					}
					comments = append(comments, scm.ReviewComment{ID: id, Author: raw.Author.Login, Path: raw.Path, Line: line, Body: raw.Body, CreatedAt: raw.CreatedAt, URL: raw.URL})
				}
				if hasNext && (next == "" || next == commentCursor) {
					return nil, errors.New("PR review comments response returned a non-progressing nested comment cursor")
				}
				commentsHasNext, commentCursor = hasNext, next
			}
		}
		if !threads.PageInfo.HasNextPage {
			break
		}
		if threads.PageInfo.EndCursor == "" || threads.PageInfo.EndCursor == cursor {
			return nil, errors.New("PR review comments response returned an invalid page cursor")
		}
		cursor = threads.PageInfo.EndCursor
	}
	return comments, nil
}

func isSupportedReviewBot(login string) bool {
	switch strings.ToLower(strings.TrimSpace(login)) {
	case "greptile-apps[bot]", "greptile-apps":
		return true
	default:
		return false
	}
}

const feedbackSnapshotQuery = `query($owner:String!,$name:String!,$number:Int!,$cursor:String,$reviewCursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){headRefOid author{login} reviewDecision reviews(first:100,after:$reviewCursor){nodes{databaseId body state submittedAt author{login}} pageInfo{hasNextPage endCursor}} reviewThreads(first:100,after:$cursor){nodes{id isResolved comments(first:100){nodes{databaseId body path line url createdAt author{login}} pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}}}}}`

const feedbackThreadCommentsQuery = `query($threadId:ID!,$cursor:String){node(id:$threadId){... on PullRequestReviewThread{comments(first:100,after:$cursor){nodes{databaseId body path line url createdAt author{login}} pageInfo{hasNextPage endCursor}}}}}`

// GetFeedback implements scm.FeedbackHost. It deliberately fetches every
// provider surface used by readiness: submitted reviews, inline review
// threads, and top-level issue comments. Missing or malformed data is an
// error, never an empty successful snapshot.
func (h *Host) GetFeedback(ctx context.Context, pr *scm.PR) (scm.FeedbackSnapshot, error) {
	if pr == nil {
		return scm.FeedbackSnapshot{}, errors.New("pr is nil")
	}
	repo := h.repoSlug()
	if repo == "" && pr.URL != "" {
		repo = RepoSlug(pr.URL)
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return scm.FeedbackSnapshot{}, fmt.Errorf("resolve GitHub feedback repository: invalid repository %q", repo)
	}
	number := strings.TrimSpace(pr.Number)
	if number == "" {
		n, err := parsePullRequestURL(pr.URL, h.host, repo)
		if err != nil {
			return scm.FeedbackSnapshot{}, err
		}
		number = strconv.Itoa(n)
	}
	prNumber, err := strconv.Atoi(number)
	if err != nil || prNumber <= 0 {
		return scm.FeedbackSnapshot{}, errors.New("expected positive GitHub pull request number")
	}

	var snapshot scm.FeedbackSnapshot
	seenItems := make(map[string]struct{})
	cursor := ""
	reviewCursor := ""
	for {
		args := []string{"api", "graphql", "-f", "query=" + feedbackSnapshotQuery, "-F", "owner=" + parts[0], "-F", "name=" + parts[1], "-F", "number=" + strconv.Itoa(prNumber)}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		if reviewCursor != "" {
			args = append(args, "-F", "reviewCursor="+reviewCursor)
		}
		if h.host != "" {
			args = append([]string{"api", "--hostname", h.host, "graphql"}, args[2:]...)
		}
		out, commandErr := h.cmd(ctx, "gh", args...).CombinedOutput()
		if commandErr != nil {
			return scm.FeedbackSnapshot{}, fmt.Errorf("gh api PR feedback: %s: %w", strings.TrimSpace(string(out)), commandErr)
		}
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						HeadRefOID string `json:"headRefOid"`
						Author     *struct {
							Login string `json:"login"`
						} `json:"author"`
						ReviewDecision string `json:"reviewDecision"`
						Reviews        struct {
							Nodes []struct {
								ID          int64     `json:"databaseId"`
								Body        string    `json:"body"`
								State       string    `json:"state"`
								SubmittedAt time.Time `json:"submittedAt"`
								Author      *struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviews"`
						ReviewThreads struct {
							Nodes []struct {
								ID         string `json:"id"`
								IsResolved bool   `json:"isResolved"`
								Comments   struct {
									Nodes []struct {
										ID        int64     `json:"databaseId"`
										Body      string    `json:"body"`
										Path      string    `json:"path"`
										URL       string    `json:"url"`
										Line      *int      `json:"line"`
										CreatedAt time.Time `json:"createdAt"`
										Author    *struct {
											Login string `json:"login"`
										} `json:"author"`
									} `json:"nodes"`
									PageInfo struct {
										HasNextPage bool   `json:"hasNextPage"`
										EndCursor   string `json:"endCursor"`
									} `json:"pageInfo"`
								} `json:"comments"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(out, &response); err != nil {
			return scm.FeedbackSnapshot{}, fmt.Errorf("decode GitHub feedback: %w", err)
		}
		if len(response.Errors) > 0 {
			return scm.FeedbackSnapshot{}, fmt.Errorf("gh api PR feedback: %s", response.Errors[0].Message)
		}
		if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
			return scm.FeedbackSnapshot{}, errors.New("GitHub feedback response did not contain the pull request")
		}
		pull := response.Data.Repository.PullRequest
		if snapshot.HeadSHA == "" {
			snapshot.HeadSHA, snapshot.ReviewDecision = pull.HeadRefOID, pull.ReviewDecision
			if pull.Author == nil || strings.TrimSpace(pull.Author.Login) == "" {
				return scm.FeedbackSnapshot{}, errors.New("GitHub feedback response omitted the pull request author")
			}
			snapshot.PRAuthor = pull.Author.Login
		} else if strings.TrimSpace(pull.HeadRefOID) != snapshot.HeadSHA {
			return scm.FeedbackSnapshot{}, fmt.Errorf("GitHub feedback head changed while paginating: %s -> %s", snapshot.HeadSHA, pull.HeadRefOID)
		}
		for _, review := range pull.Reviews.Nodes {
			if review.Author == nil || strings.TrimSpace(review.State) == "PENDING" {
				continue
			}
			id := strconv.FormatInt(review.ID, 10)
			if _, seen := seenItems[id]; seen {
				continue
			}
			seenItems[id] = struct{}{}
			snapshot.Items = append(snapshot.Items, scm.FeedbackItem{ID: id, Kind: scm.FeedbackReview, ReviewState: review.State, Author: review.Author.Login, AuthorIsBot: strings.HasSuffix(strings.ToLower(review.Author.Login), "[bot]"), Body: review.Body, CreatedAt: review.SubmittedAt})
		}
		for _, thread := range pull.ReviewThreads.Nodes {
			comments := thread.Comments.Nodes
			commentCursor := thread.Comments.PageInfo.EndCursor
			commentsHasNext := thread.Comments.PageInfo.HasNextPage
			for _, comment := range comments {
				if comment.Author == nil {
					continue
				}
				id := strconv.FormatInt(comment.ID, 10)
				if _, seen := seenItems[id]; seen {
					continue
				}
				seenItems[id] = struct{}{}
				line := 0
				if comment.Line != nil {
					line = *comment.Line
				}
				snapshot.Items = append(snapshot.Items, scm.FeedbackItem{ID: id, ThreadID: thread.ID, Kind: scm.FeedbackInlineReview, URL: comment.URL, Author: comment.Author.Login, AuthorIsBot: strings.HasSuffix(strings.ToLower(comment.Author.Login), "[bot]"), Body: comment.Body, Path: comment.Path, Line: line, CreatedAt: comment.CreatedAt, Resolved: thread.IsResolved})
			}
			for commentsHasNext {
				if commentCursor == "" {
					return scm.FeedbackSnapshot{}, errors.New("GitHub feedback response returned an invalid nested comment cursor")
				}
				more, next, hasNext, fetchErr := h.getFeedbackThreadComments(ctx, thread.ID, commentCursor)
				if fetchErr != nil {
					return scm.FeedbackSnapshot{}, fetchErr
				}
				for _, comment := range more {
					if comment.Author == nil {
						continue
					}
					id := strconv.FormatInt(comment.ID, 10)
					if _, seen := seenItems[id]; seen {
						continue
					}
					seenItems[id] = struct{}{}
					line := 0
					if comment.Line != nil {
						line = *comment.Line
					}
					snapshot.Items = append(snapshot.Items, scm.FeedbackItem{ID: id, ThreadID: thread.ID, Kind: scm.FeedbackInlineReview, URL: comment.URL, Author: comment.Author.Login, AuthorIsBot: strings.HasSuffix(strings.ToLower(comment.Author.Login), "[bot]"), Body: comment.Body, Path: comment.Path, Line: line, CreatedAt: comment.CreatedAt, Resolved: thread.IsResolved})
				}
				commentsHasNext = hasNext
				if commentsHasNext && (next == "" || next == commentCursor) {
					return scm.FeedbackSnapshot{}, errors.New("GitHub feedback response returned a non-progressing nested comment cursor")
				}
				commentCursor = next
			}
		}
		if pull.Reviews.PageInfo.HasNextPage {
			if pull.Reviews.PageInfo.EndCursor == "" || pull.Reviews.PageInfo.EndCursor == reviewCursor {
				return scm.FeedbackSnapshot{}, errors.New("GitHub feedback response returned an invalid review cursor")
			}
			reviewCursor = pull.Reviews.PageInfo.EndCursor
		}
		pageInfo := pull.ReviewThreads.PageInfo
		if !pageInfo.HasNextPage && !pull.Reviews.PageInfo.HasNextPage {
			break
		}
		if pageInfo.HasNextPage {
			if pageInfo.EndCursor == "" || pageInfo.EndCursor == cursor {
				return scm.FeedbackSnapshot{}, errors.New("GitHub feedback response returned an invalid review-thread page cursor")
			}
			cursor = pageInfo.EndCursor
		}
	}
	issueItems, err := h.getIssueFeedback(ctx, repo, number)
	if err != nil {
		return scm.FeedbackSnapshot{}, err
	}
	for _, item := range issueItems {
		if _, seen := seenItems[item.ID]; seen {
			continue
		}
		seenItems[item.ID] = struct{}{}
		snapshot.Items = append(snapshot.Items, item)
	}
	return snapshot, nil
}

func (h *Host) getFeedbackThreadComments(ctx context.Context, threadID, cursor string) ([]struct {
	ID        int64     `json:"databaseId"`
	Body      string    `json:"body"`
	Path      string    `json:"path"`
	URL       string    `json:"url"`
	Line      *int      `json:"line"`
	CreatedAt time.Time `json:"createdAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
}, string, bool, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "graphql", "-f", "query="+feedbackThreadCommentsQuery, "-F", "threadId="+threadID, "-F", "cursor="+cursor)
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, "", false, fmt.Errorf("gh api nested PR comments: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var response struct {
		Data struct {
			Node *struct {
				Comments struct {
					Nodes []struct {
						ID        int64     `json:"databaseId"`
						Body      string    `json:"body"`
						Path      string    `json:"path"`
						URL       string    `json:"url"`
						Line      *int      `json:"line"`
						CreatedAt time.Time `json:"createdAt"`
						Author    *struct {
							Login string `json:"login"`
						} `json:"author"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"comments"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, "", false, fmt.Errorf("decode nested PR comments: %w", err)
	}
	if len(response.Errors) > 0 {
		return nil, "", false, errors.New(response.Errors[0].Message)
	}
	if response.Data.Node == nil {
		return nil, "", false, errors.New("nested PR comments response omitted thread")
	}
	return response.Data.Node.Comments.Nodes, response.Data.Node.Comments.PageInfo.EndCursor, response.Data.Node.Comments.PageInfo.HasNextPage, nil
}

type githubIssueComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	User      *struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (h *Host) getIssueFeedback(ctx context.Context, repo, number string) ([]scm.FeedbackItem, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "repos/"+repo+"/issues/"+number+"/comments", "--paginate", "--slurp")
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api PR issue comments: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var pages [][]githubIssueComment
	if err := json.Unmarshal(out, &pages); err != nil {
		var flat []githubIssueComment
		if flatErr := json.Unmarshal(out, &flat); flatErr != nil {
			return nil, fmt.Errorf("decode GitHub issue comments: %w", err)
		}
		pages = [][]githubIssueComment{flat}
	}
	items := make([]scm.FeedbackItem, 0)
	for _, page := range pages {
		for _, comment := range page {
			if comment.User == nil {
				continue
			}
			items = append(items, scm.FeedbackItem{ID: strconv.FormatInt(comment.ID, 10), Kind: scm.FeedbackIssueComment, URL: comment.HTMLURL, Author: comment.User.Login, AuthorIsBot: strings.EqualFold(comment.User.Type, "Bot") || strings.HasSuffix(strings.ToLower(comment.User.Login), "[bot]"), Body: comment.Body, CreatedAt: comment.CreatedAt})
		}
	}
	return items, nil
}

// ReplyToFeedback publishes a factual, head-bound response. Inline replies use
// GitHub's review-comment reply endpoint; issue comments are ordinary PR
// comments. Review submissions are intentionally not writable here because a
// disposition must never masquerade as a reviewer decision.
func (h *Host) ReplyToFeedback(ctx context.Context, pr *scm.PR, item scm.FeedbackItem, body string) error {
	if pr == nil || strings.TrimSpace(body) == "" {
		return errors.New("cannot reply to empty feedback or nil PR")
	}
	repo := h.repoSlug()
	if repo == "" {
		repo = RepoSlug(pr.URL)
	}
	if repo == "" || strings.TrimSpace(item.ID) == "" {
		return errors.New("cannot determine repository or feedback ID")
	}
	path := "repos/" + repo + "/issues/" + strings.TrimSpace(pr.Number) + "/comments"
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	if item.Kind == scm.FeedbackInlineReview {
		if strings.TrimSpace(pr.Number) == "" {
			return errors.New("cannot reply to inline feedback without pull request number")
		}
		path = "repos/" + repo + "/pulls/" + strings.TrimSpace(pr.Number) + "/comments/" + item.ID + "/replies"
	}
	args = append(args, path, "--method", "POST", "-f", "body="+body)
	if out, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("publish GitHub feedback reply: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ResolveFeedback resolves only inline review threads. Top-level comments and
// submitted reviews have no provider resolution primitive in this contract.
func (h *Host) ResolveFeedback(ctx context.Context, pr *scm.PR, item scm.FeedbackItem) error {
	if item.Kind != scm.FeedbackInlineReview || strings.TrimSpace(item.ThreadID) == "" {
		return scm.ErrUnsupported
	}
	query := `mutation($threadId:ID!){resolveReviewThread(input:{threadId:$threadId}){thread{isResolved}}}`
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "graphql", "-f", "query="+query, "-F", "threadId="+item.ThreadID)
	if out, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("resolve GitHub feedback thread: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
