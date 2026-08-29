package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDecodeProofFindingsRejectsWeakPayloads(t *testing.T) {
	for _, raw := range []string{`{}`, `{"summary":"ok"}`, `{"findings":[],"summary":""}`, `{"findings":[{}],"summary":"ok"}`} {
		if _, err := decodeProofFindings(&agent.Result{Output: json.RawMessage(raw)}); err == nil {
			t.Fatalf("payload %s unexpectedly accepted", raw)
		}
	}
}

func TestProofArtifactsRejectStaleEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(`{"fresh":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proofArtifacts(dir, info.ModTime().Unix()+2); err == nil {
		t.Fatal("stale evidence unexpectedly accepted")
	}
}

func TestAllStepsWithProofPlacesIndependentGatesBeforeDocument(t *testing.T) {
	steps := AllStepsWithProof()
	want := []types.StepName{
		types.StepIntent, types.StepRebase, types.StepReview, types.StepTest,
		types.StepProof, types.StepProofReview, types.StepDocument, types.StepLint,
		types.StepPush, types.StepPR, types.StepCI,
	}
	if len(steps) != len(want) {
		t.Fatalf("proof-aware step count = %d, want %d", len(steps), len(want))
	}
	for i, step := range steps {
		if got := step.Name(); got != want[i] {
			t.Fatalf("step %d = %q, want %q", i, got, want[i])
		}
	}
}
