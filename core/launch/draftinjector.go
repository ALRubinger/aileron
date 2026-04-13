package launch

import (
	"fmt"
	"io"
)

// DraftInjector writes draft-request prompts into the agent's pty stdin.
// When the agent reads from stdin, it sees the prompt as user input and
// can use the send_message MCP tool to draft a reply.
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
		"A teammate sent a message in %s. %s says: %q Please draft a reply using the send_message tool with service=%q and channel=%q.\n",
		msg.Channel, msg.Author, msg.Body, msg.Source, msg.Channel,
	)
	di.ptmx.Write([]byte(prompt))
}

// InjectRevision writes a revision prompt that includes the original
// message, the current draft, and the user's feedback so the agent can
// produce an improved draft.
func (di *DraftInjector) InjectRevision(msg Message, feedback string) {
	prompt := fmt.Sprintf(
		"A teammate sent a message in %s. %s says: %q Your previous draft reply was: %q The user wants you to revise. Feedback: %q Please draft a new reply using the send_message tool with service=%q and channel=%q.\n",
		msg.Channel, msg.Author, msg.Body, msg.Draft, feedback, msg.Source, msg.Channel,
	)
	di.ptmx.Write([]byte(prompt))
}
