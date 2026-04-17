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

func TestLoadSystemPrompt_FallbackWhenMissing(t *testing.T) {
	t.Setenv("AILERON_PROMPT_FILE", "/nonexistent/AILERON.md")

	got := LoadSystemPrompt()
	if got != defaultSystemPrompt {
		t.Error("expected default prompt when file is missing")
	}
}

func TestLoadSystemPrompt_FallbackWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AILERON.md")
	os.WriteFile(path, []byte("   \n  "), 0644)

	t.Setenv("AILERON_PROMPT_FILE", path)

	got := LoadSystemPrompt()
	if got != defaultSystemPrompt {
		t.Error("expected default prompt when file is empty/whitespace")
	}
}

func TestLoadSystemPrompt_DefaultPath(t *testing.T) {
	// Unset the env var — should look for AILERON.md in working dir.
	t.Setenv("AILERON_PROMPT_FILE", "")

	// From the test working directory, AILERON.md doesn't exist,
	// so it should fall back to the default.
	got := LoadSystemPrompt()
	if got != defaultSystemPrompt {
		t.Error("expected default prompt when AILERON.md not in working dir")
	}
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
