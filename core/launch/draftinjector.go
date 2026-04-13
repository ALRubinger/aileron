package launch

import (
	"fmt"
	"io"
)

// DraftInjector writes draft-request prompts into the agent's pty stdin.
// When the agent reads from stdin, it sees the prompt as user input and
// uses the draft_reply MCP tool to submit draft text for user review.
// Aileron handles sending — the agent only drafts.
type DraftInjector struct {
	ptmx io.Writer
}

// NewDraftInjector creates an injector that writes to the given pty master.
func NewDraftInjector(ptmx io.Writer) *DraftInjector {
	return &DraftInjector{ptmx: ptmx}
}

// Inject writes a draft-request prompt for the given message into the
// agent's pty. The trailing newline submits it as user input.
func (di *DraftInjector) Inject(msg Message) {
	prompt := fmt.Sprintf(
		"I got this message from %s in %s: %q — draft a reply for me using draft_reply with message_id=%q\n",
		msg.Author, msg.Channel, msg.Body, msg.ID,
	)
	di.ptmx.Write([]byte(prompt))
}

// InjectRevision writes a revision prompt that includes the user's
// feedback so the agent can produce an improved draft.
func (di *DraftInjector) InjectRevision(msg Message, feedback string) {
	prompt := fmt.Sprintf(
		"Revise the draft reply to message %q — %s. Use draft_reply with your revised text.\n",
		msg.ID, feedback,
	)
	di.ptmx.Write([]byte(prompt))
}
