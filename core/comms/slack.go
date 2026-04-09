package comms

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// SlackListener implements Listener for Slack using Socket Mode.
// It connects via WebSocket (no public URL required) and delivers
// incoming messages to a channel.
type SlackListener struct {
	appToken string
	botToken string
	channels map[string]bool // channel names to listen on
	ignore   map[string]bool // channel names to ignore

	api    *slack.Client
	socket *socketmode.Client
}

// NewSlackListener creates a Slack listener with the given tokens and
// channel configuration.
func NewSlackListener(appToken, botToken string, channels, ignore []string) *SlackListener {
	chMap := make(map[string]bool, len(channels))
	for _, ch := range channels {
		chMap[ch] = true
	}
	igMap := make(map[string]bool, len(ignore))
	for _, ch := range ignore {
		igMap[ch] = true
	}
	return &SlackListener{
		appToken: appToken,
		botToken: botToken,
		channels: chMap,
		ignore:   igMap,
	}
}

func (s *SlackListener) Service() string { return "slack" }

// Connect initializes the Slack API and Socket Mode clients.
func (s *SlackListener) Connect(ctx context.Context) error {
	if s.appToken == "" || s.botToken == "" {
		return fmt.Errorf("slack: app_token and bot_token are required")
	}

	s.api = slack.New(
		s.botToken,
		slack.OptionAppLevelToken(s.appToken),
	)
	s.socket = socketmode.New(
		s.api,
		socketmode.OptionLog(log.New(log.Writer(), "slack-socket: ", log.LstdFlags)),
	)
	return nil
}

// Listen starts the Socket Mode event loop and delivers incoming
// messages to the returned channel. Blocks until ctx is cancelled.
func (s *SlackListener) Listen(ctx context.Context) (<-chan IncomingMessage, error) {
	if s.socket == nil {
		return nil, fmt.Errorf("slack: not connected (call Connect first)")
	}

	msgs := make(chan IncomingMessage, 100)

	// Start the Socket Mode handler in the background.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-s.socket.Events:
				if !ok {
					return
				}
				s.handleEvent(evt, msgs)
			}
		}
	}()

	// Run the Socket Mode connection (blocks until ctx cancelled or error).
	go func() {
		_ = s.socket.RunContext(ctx)
		close(msgs)
	}()

	return msgs, nil
}

func (s *SlackListener) handleEvent(evt socketmode.Event, msgs chan<- IncomingMessage) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		s.socket.Ack(*evt.Request)

		switch innerEvt := eventsAPIEvent.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			// Skip bot messages to avoid feedback loops.
			if innerEvt.BotID != "" {
				return
			}
			channel := innerEvt.Channel
			if s.ignore[channel] {
				return
			}
			// If specific channels are configured, only listen on those.
			if len(s.channels) > 0 && !s.channels[channel] {
				return
			}

			// Resolve channel name (the event gives us an ID).
			channelName := channel
			if info, err := s.api.GetConversationInfo(&slack.GetConversationInfoInput{
				ChannelID: channel,
			}); err == nil && info != nil {
				channelName = "#" + info.Name
			}

			// Resolve user name.
			author := innerEvt.User
			if user, err := s.api.GetUserInfo(innerEvt.User); err == nil {
				author = user.RealName
				if author == "" {
					author = user.Name
				}
			}

			body := innerEvt.Text
			preview := body
			if len(preview) > 80 {
				preview = preview[:77] + "..."
			}

			msgs <- IncomingMessage{
				ID:        innerEvt.TimeStamp,
				Service:   "slack",
				Channel:   channelName,
				Author:    author,
				Body:      body,
				Timestamp: time.Now(),
			}
		}
	default:
		// Acknowledge other event types we don't handle.
		if evt.Request != nil {
			s.socket.Ack(*evt.Request)
		}
	}
}

// Send posts a message to the given Slack channel.
func (s *SlackListener) Send(ctx context.Context, msg OutgoingMessage) error {
	if s.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	_, _, err := s.api.PostMessageContext(ctx, msg.Channel,
		slack.MsgOptionText(msg.Body, false),
	)
	return err
}

// Close shuts down the Socket Mode connection.
func (s *SlackListener) Close() error {
	// The socket mode client is stopped by cancelling the context
	// passed to RunContext. No explicit close needed.
	return nil
}
