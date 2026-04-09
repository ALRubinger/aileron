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

		if innerEvt, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.MessageEvent); ok {
			if msg, deliver := s.ProcessMessageEvent(innerEvt); deliver {
				msgs <- msg
			}
		}
	default:
		if evt.Request != nil {
			s.socket.Ack(*evt.Request)
		}
	}
}

// ProcessMessageEvent converts a Slack message event into an
// IncomingMessage. Exported for testing. Returns the message and true
// if the event should be delivered, or false if it should be skipped
// (bot message, ignored channel, etc.).
func (s *SlackListener) ProcessMessageEvent(evt *slackevents.MessageEvent) (IncomingMessage, bool) {
	if evt.BotID != "" {
		return IncomingMessage{}, false
	}
	channel := evt.Channel
	if s.ignore[channel] {
		return IncomingMessage{}, false
	}
	if len(s.channels) > 0 && !s.channels[channel] {
		return IncomingMessage{}, false
	}

	channelName := s.resolveChannelName(channel)
	author := s.resolveAuthor(evt.User)

	return BuildIncomingMessage(evt.TimeStamp, channelName, author, evt.Text), true
}

func (s *SlackListener) resolveChannelName(id string) string {
	if s.api != nil {
		if info, err := s.api.GetConversationInfo(&slack.GetConversationInfoInput{
			ChannelID: id,
		}); err == nil && info != nil {
			return "#" + info.Name
		}
	}
	return id
}

func (s *SlackListener) resolveAuthor(userID string) string {
	if s.api != nil {
		if user, err := s.api.GetUserInfo(userID); err == nil {
			if user.RealName != "" {
				return user.RealName
			}
			return user.Name
		}
	}
	return userID
}

// BuildIncomingMessage constructs an IncomingMessage from raw fields.
// Exported for testing the message construction independent of API calls.
func BuildIncomingMessage(ts, channel, author, text string) IncomingMessage {
	preview := text
	if len(preview) > 80 {
		preview = preview[:77] + "..."
	}
	return IncomingMessage{
		ID:        ts,
		Service:   "slack",
		Channel:   channel,
		Author:    author,
		Body:      text,
		Timestamp: time.Now(),
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
