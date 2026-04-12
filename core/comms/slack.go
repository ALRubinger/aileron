package comms

import (
	"context"
	"fmt"
	"log"
	"log/slog"
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
	log      *slog.Logger

	// apiURL overrides the Slack API base URL for testing.
	apiURL string

	api    *slack.Client
	socket *socketmode.Client
}

// SetAPIURL overrides the Slack API base URL. Call before Connect.
// Used in integration tests with a mock HTTP server.
func (s *SlackListener) SetAPIURL(url string) {
	s.apiURL = url
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

// SetLogger sets an optional logger for verbose diagnostics.
func (s *SlackListener) SetLogger(l *slog.Logger) { s.log = l }

func (s *SlackListener) debug(msg string, args ...any) {
	if s.log != nil {
		s.log.Debug(msg, args...)
	}
}

// Connect initializes the Slack API and Socket Mode clients.
func (s *SlackListener) Connect(ctx context.Context) error {
	if s.appToken == "" || s.botToken == "" {
		return fmt.Errorf("slack: app_token and bot_token are required")
	}

	opts := []slack.Option{
		slack.OptionAppLevelToken(s.appToken),
	}
	if s.apiURL != "" {
		opts = append(opts, slack.OptionAPIURL(s.apiURL))
	}
	s.api = slack.New(s.botToken, opts...)
	s.socket = socketmode.New(
		s.api,
		socketmode.OptionLog(log.New(log.Writer(), "slack-socket: ", log.LstdFlags)),
	)

	s.debug("slack: connected", "channels", len(s.channels))
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

	s.debug("slack: listening via Socket Mode")
	return msgs, nil
}

func (s *SlackListener) handleEvent(evt socketmode.Event, msgs chan<- IncomingMessage) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			s.debug("slack: received EventsAPI event with unexpected data type")
			return
		}
		s.socket.Ack(*evt.Request)

		if innerEvt, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.MessageEvent); ok {
			s.debug("slack: received message event", "channel", innerEvt.Channel, "user", innerEvt.User)
			if msg, deliver := s.ProcessMessageEvent(innerEvt); deliver {
				msgs <- msg
			}
		} else {
			s.debug("slack: received non-message event", "type", eventsAPIEvent.InnerEvent.Type)
		}
	case socketmode.EventTypeConnectionError:
		s.debug("slack: connection error", "event", fmt.Sprintf("%+v", evt.Data))
	case socketmode.EventTypeConnecting:
		s.debug("slack: connecting to Socket Mode")
	case socketmode.EventTypeConnected:
		s.debug("slack: Socket Mode connection established")
	case socketmode.EventTypeDisconnect:
		s.debug("slack: disconnected from Socket Mode")
	case socketmode.EventTypeHello:
		s.debug("slack: received hello from Socket Mode")
	default:
		s.debug("slack: unhandled event", "type", evt.Type)
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
		s.debug("slack: skipping bot message", "bot_id", evt.BotID, "channel", evt.Channel)
		return IncomingMessage{}, false
	}

	// Slack delivers channel IDs (C07ABC...) but aileron.yaml configures
	// channel names (#backend). Resolve the ID to a name before filtering
	// so both forms work in the config.
	channelID := evt.Channel
	channelName := s.resolveChannelName(channelID)

	if s.matchChannel(channelID, channelName, s.ignore) {
		s.debug("slack: skipping ignored channel", "channel", channelName, "id", channelID)
		return IncomingMessage{}, false
	}
	if len(s.channels) > 0 && !s.matchChannel(channelID, channelName, s.channels) {
		s.debug("slack: skipping unconfigured channel", "channel", channelName, "id", channelID)
		return IncomingMessage{}, false
	}

	author := s.resolveAuthor(evt.User)

	s.debug("slack: delivering message", "channel", channelName, "author", author)
	return BuildIncomingMessage(evt.TimeStamp, channelName, author, evt.Text), true
}

// matchChannel checks if a channel matches any entry in the map by ID or name.
func (s *SlackListener) matchChannel(id, name string, m map[string]bool) bool {
	return m[id] || m[name]
}

func (s *SlackListener) resolveChannelName(id string) string {
	if s.api != nil {
		info, err := s.api.GetConversationInfo(&slack.GetConversationInfoInput{
			ChannelID: id,
		})
		if err != nil {
			s.debug("slack: failed to resolve channel name", "channel_id", id, "error", err)
		} else if info != nil {
			return "#" + info.Name
		}
	}
	return id
}

func (s *SlackListener) resolveAuthor(userID string) string {
	if s.api != nil {
		user, err := s.api.GetUserInfo(userID)
		if err != nil {
			s.debug("slack: failed to resolve user", "user_id", userID, "error", err)
		} else {
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
