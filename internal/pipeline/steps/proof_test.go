package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
