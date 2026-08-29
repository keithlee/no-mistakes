package steps

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const verifyPyRelPath = "../../../.github/actions/require-no-mistakes/verify.py"

func compliantPipelineBody(t *testing.T, headSHA string) string {
	t.Helper()
	stepResults := []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	rounds := make(map[string][]*db.StepRound, len(stepResults))
	for _, sr := range stepResults {
		rounds[sr.ID] = []*db.StepRound{{Round: 1, Trigger: "initial", DurationMS: 1}}
	}
	md, _ := BuildPipelineSummary(stepResults, rounds, headSHA)
	if md == "" {
		t.Fatal("BuildPipelineSummary returned empty markdown")
	}
	return md
}

func pythonInterpreterForVerify(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no python interpreter available to execute verify.py")
	return ""
}

func runVerifyPy(t *testing.T, body, headSHA string) (conclusion, output string) {
	t.Helper()
	python := pythonInterpreterForVerify(t)
	outputFile := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
		t.Fatalf("seed GITHUB_OUTPUT: %v", err)
	}
	cmd := exec.Command(python, verifyPyRelPath)
	cmd.Env = append(os.Environ(),
		"PR_BODY="+body,
		"PR_HEAD_SHA="+headSHA,
		"PR_NUMBER=42",
		"GITHUB_OUTPUT="+outputFile,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	switch {
	case err == nil:
		return "success", buf.String()
	default:
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("execute verify.py: %v\n%s", err, buf.String())
		}
		return "failure", buf.String()
	}
}

func TestRebindPipelineAttestationHead_VerifyPyRoundTrip(t *testing.T) {
	t.Parallel()
	originalHead := testPipelineHeadSHA
	repairHead := strings.Repeat("ab", 20)
	original := compliantPipelineBody(t, originalHead)

	if got, out := runVerifyPy(t, original, originalHead); got != "success" {
		t.Fatalf("original body at original head: conclusion=%s\n%s", got, out)
	}
	if got, out := runVerifyPy(t, original, repairHead); got != "failure" || !strings.Contains(out, "does not match") {
		t.Fatalf("stale attestation at new head must fail the head bind, got %s\n%s", got, out)
	}

	rebound, ok := rebindPipelineAttestationHead(original, repairHead)
	if !ok {
		t.Fatal("expected a live attestation to rebind")
	}
	if parsePipelineAttestationForTest(t, rebound).HeadSHA != repairHead {
		t.Fatalf("rebound head = %q, want %q", parsePipelineAttestationForTest(t, rebound).HeadSHA, repairHead)
	}
	if got, out := runVerifyPy(t, rebound, repairHead); got != "success" {
		t.Fatalf("rebound attestation at the new head must pass, got %s\n%s", got, out)
	}

	foreign := "a regular pull request with no pipeline section"
	unchanged, ok := rebindPipelineAttestationHead(foreign, repairHead)
	if ok {
		t.Fatal("rebind must not mint an attestation for a PR that was not raised through no-mistakes")
	}
	if unchanged != foreign {
		t.Fatal("body without an attestation must be left untouched")
	}
	if got, out := runVerifyPy(t, unchanged, repairHead); got != "failure" || !strings.Contains(out, "not raised through no-mistakes") {
		t.Fatalf("a PR not raised through no-mistakes must still fail, got %s\n%s", got, out)
	}
}

type attestationTestHost struct {
	scm.Host
	title   string
	body    string
	updated scm.PRContent
	updates int
}

func (h *attestationTestHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	return scm.PRContent{Title: h.title, Body: h.body}, nil
}

func (h *attestationTestHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.updates++
	h.updated = content
	h.body = content.Body
	return pr, nil
}

func TestRestampPRAttestation_RebindsExistingAndSkipsMissing(t *testing.T) {
	t.Parallel()
	originalHead := testPipelineHeadSHA
	repairHead := strings.Repeat("cd", 20)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}

	t.Run("existing_attestation_is_rebound", func(t *testing.T) {
		t.Parallel()
		host := &attestationTestHost{title: "fix: ci", body: compliantPipelineBody(t, originalHead)}
		if err := restampPRAttestation(context.Background(), host, pr, repairHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 1 {
			t.Fatalf("UpdatePR calls = %d, want 1", host.updates)
		}
		if parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA != repairHead {
			t.Fatalf("updated attestation head = %q, want %q", parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA, repairHead)
		}
		if got, out := runVerifyPy(t, host.updated.Body, repairHead); got != "success" {
			t.Fatalf("restamped body must pass verify.py at the new head, got %s\n%s", got, out)
		}

		secondHead := strings.Repeat("ef", 20)
		if err := restampPRAttestation(context.Background(), host, pr, secondHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 2 {
			t.Fatalf("UpdatePR calls after a second repair = %d, want 2", host.updates)
		}
		if parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA != secondHead {
			t.Fatalf("second restamp head = %q, want %q", parsePipelineAttestationForTest(t, host.updated.Body).HeadSHA, secondHead)
		}
		if got, out := runVerifyPy(t, host.updated.Body, secondHead); got != "success" {
			t.Fatalf("attestation must stay valid across a further repair push, got %s\n%s", got, out)
		}
		if got, out := runVerifyPy(t, host.updated.Body, originalHead); got != "failure" {
			t.Fatalf("a restamped attestation must not still bind the original head, got %s\n%s", got, out)
		}
	})

	t.Run("missing_attestation_is_not_invented", func(t *testing.T) {
		t.Parallel()
		const foreign = "a regular pull request with no pipeline section"
		host := &attestationTestHost{title: "feat: hand rolled", body: foreign}
		if err := restampPRAttestation(context.Background(), host, pr, repairHead, nil); err != nil {
			t.Fatal(err)
		}
		if host.updates != 0 {
			t.Fatalf("UpdatePR calls = %d, want 0 (must not mint an attestation)", host.updates)
		}
		if host.body != foreign {
			t.Fatal("body without an attestation must be left untouched")
		}
		if got, out := runVerifyPy(t, host.body, repairHead); got != "failure" {
			t.Fatalf("a PR not raised through no-mistakes must still fail, got %s\n%s", got, out)
		}
	})
}

func TestCIStep_PublishRepairRebindsAttestationAcrossRepairPushes(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	original := compliantPipelineBody(t, f.headSHA)
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	f.sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	env := fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
	f.sctx.Env = append(env,
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=fix: ci",
		"FAKE_CLI_LOG="+logFile,
	)
	f.sctx.Ctx = context.Background()
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("commitRepair: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced || repair.Revalidate {
		t.Fatalf("repair = %+v, want a published head advance", repair)
	}
	newHead := f.localHead(t)
	if newHead == f.headSHA {
		t.Fatal("expected a new repair commit")
	}

	updated := readFakeGHBodyArg(t, logFile)
	if parsePipelineAttestationForTest(t, updated).HeadSHA != newHead {
		t.Fatalf("published attestation head = %q, want the repair commit %q", parsePipelineAttestationForTest(t, updated).HeadSHA, newHead)
	}
	if got, out := runVerifyPy(t, updated, newHead); got != "success" {
		t.Fatalf("attestation after a repair push must stay valid, got %s\n%s", got, out)
	}
	if got, out := runVerifyPy(t, original, newHead); got != "failure" {
		t.Fatalf("the pre-repair attestation must fail at the new head, got %s\n%s", got, out)
	}
}

func TestCIStep_PublishRepairDoesNotMintAttestation(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	const foreign = "a regular pull request with no pipeline section"
	bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "gh.log")
	f.sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	env := fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
	f.sctx.Env = append(env,
		"FAKE_CLI_PR_BODY_FILE="+bodyFile,
		"FAKE_CLI_PR_TITLE=feat: hand rolled",
		"FAKE_CLI_LOG="+logFile,
	)
	f.sctx.Ctx = context.Background()
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("commitRepair: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced {
		t.Fatal("expected the repair to publish")
	}
	if logData, err := os.ReadFile(logFile); err == nil && strings.Contains(string(logData), "stdin --body ") {
		t.Fatalf("must not write a PR body when no attestation was present:\n%s", logData)
	}
	newHead := f.localHead(t)
	if got, out := runVerifyPy(t, foreign, newHead); got != "failure" {
		t.Fatalf("a PR not raised through no-mistakes must still fail after a push, got %s\n%s", got, out)
	}
}
