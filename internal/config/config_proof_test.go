package config

import "testing"

func TestProofGuidanceIsGlobalOnly(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("proof:\n  guidance_files:\n    - /tmp/operator-proof.md\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("proof:\n  guidance_files:\n    - /tmp/branch-injected.md\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(global, repo)
	if len(merged.Proof.GuidanceFiles) != 1 || merged.Proof.GuidanceFiles[0] != "/tmp/operator-proof.md" {
		t.Fatalf("proof guidance = %#v", merged.Proof.GuidanceFiles)
	}
}

func TestProofGuidanceRejectsRelativeAndOversizedConfiguration(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("proof:\n  guidance_files: [guidance.md]\n")); err == nil {
		t.Fatal("relative proof guidance accepted")
	}
	files := make([]string, maxProofGuidanceFiles+1)
	for i := range files {
		files[i] = "/tmp/proof-" + string(rune('a'+i)) + ".md"
	}
	data := "proof:\n  guidance_files:\n"
	for _, file := range files {
		data += "    - " + file + "\n"
	}
	if _, err := LoadGlobalFromBytes([]byte(data)); err == nil {
		t.Fatal("oversized proof guidance accepted")
	}
}
