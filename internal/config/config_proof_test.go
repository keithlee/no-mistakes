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

func TestFeedbackPolicyDefaultsToStrictAllBotsAndIsGloballyConfigurable(t *testing.T) {
	defaults, err := LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Feedback.IncludeBots == nil || !*defaults.Feedback.IncludeBots || len(defaults.Feedback.BotAuthorPatterns) != 1 || defaults.Feedback.BotAuthorPatterns[0] != "*" {
		t.Fatalf("default feedback policy = %#v", defaults.Feedback)
	}
	configured, err := LoadGlobalFromBytes([]byte("feedback:\n  include_bots: false\n  bot_author_patterns: [greptile*]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if configured.Feedback.IncludeBots == nil || *configured.Feedback.IncludeBots || configured.Feedback.BotAuthorPatterns[0] != "greptile*" {
		t.Fatalf("configured feedback policy = %#v", configured.Feedback)
	}
}

func TestFeedbackPolicyRejectsInvalidPattern(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("feedback:\n  bot_author_patterns: ['[']\n")); err == nil {
		t.Fatal("invalid feedback author pattern accepted")
	}
}
