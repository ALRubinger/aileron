package draft

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSystemPrompt_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	os.WriteFile(path, []byte("# Custom Prompt\n\nBe helpful."), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)

	got := LoadSystemPrompt()
	if got != "# Custom Prompt\n\nBe helpful." {
		t.Errorf("expected custom prompt, got %q", got)
	}
}

func TestLoadSystemPrompt_PanicsWhenMissing(t *testing.T) {
	t.Setenv("AILERON_PROMPT_FILE", "/nonexistent/AILERON.md")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when AILERON.md is missing")
		}
	}()
	LoadSystemPrompt()
}

func TestLoadSystemPrompt_PanicsWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	os.WriteFile(path, []byte("   \n  "), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when AILERON.md is empty")
		}
	}()
	LoadSystemPrompt()
}

func TestLoadSystemPrompt_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	os.WriteFile(path, []byte("\n  Custom prompt content  \n\n"), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)

	got := LoadSystemPrompt()
	if got != "Custom prompt content" {
		t.Errorf("expected trimmed content, got %q", got)
	}
}

func TestLoadSystemPrompt_SetsPromptSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	os.WriteFile(path, []byte("test prompt"), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)
	LoadSystemPrompt()

	if PromptSource() != path {
		t.Errorf("expected source %q, got %q", path, PromptSource())
	}
}

func TestLoadPrompts_TwoSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	content := "# Research Prompt\n\nGather context.\n\n---\n\n# Ghostwrite Prompt\n\nWrite the reply."
	os.WriteFile(path, []byte(content), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)

	prompts := LoadPrompts()
	if prompts.Research != "# Research Prompt\n\nGather context." {
		t.Errorf("unexpected research prompt: %q", prompts.Research)
	}
	if prompts.Ghostwrite != "# Ghostwrite Prompt\n\nWrite the reply." {
		t.Errorf("unexpected ghostwrite prompt: %q", prompts.Ghostwrite)
	}
}

func TestLoadPrompts_SingleSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	os.WriteFile(path, []byte("Single section prompt"), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)

	prompts := LoadPrompts()
	if prompts.Research != "" {
		t.Errorf("expected empty research prompt, got %q", prompts.Research)
	}
	if prompts.Ghostwrite != "Single section prompt" {
		t.Errorf("expected single section as ghostwrite, got %q", prompts.Ghostwrite)
	}
}
