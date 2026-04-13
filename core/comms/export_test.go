package comms

import "github.com/slack-go/slack/socketmode"

// HandleEvent exposes handleEvent for testing.
func (s *SlackListener) HandleEvent(evt socketmode.Event, msgs chan<- IncomingMessage) {
	s.handleEvent(evt, msgs)
}
