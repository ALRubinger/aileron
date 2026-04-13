package comms

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

// DiscordListener implements Listener for Discord using the Gateway WebSocket.
// It connects via a bot token and delivers incoming messages to a channel.
type DiscordListener struct {
	botToken string
	channels map[string]bool // channel IDs to listen on
	ignore   map[string]bool // channel IDs to ignore
	log      *slog.Logger

	// openGateway opens the Discord WebSocket connection. Defaults to
	// session.Open but can be overridden for testing.
	openGateway func() error

	session *discordgo.Session
	msgs    chan IncomingMessage
}

// NewDiscordListener creates a Discord listener with the given bot token
// and channel configuration.
func NewDiscordListener(botToken string, channels, ignore []string, log *slog.Logger) *DiscordListener {
	chMap := make(map[string]bool, len(channels))
	for _, ch := range channels {
		chMap[ch] = true
	}
	igMap := make(map[string]bool, len(ignore))
	for _, ch := range ignore {
		igMap[ch] = true
	}
	return &DiscordListener{
		botToken: botToken,
		channels: chMap,
		ignore:   igMap,
		log:      log,
	}
}

func (d *DiscordListener) Service() string { return "discord" }

// SetOpenGateway overrides the function used to open the Discord Gateway
// connection. Call after Connect. Used in tests to skip the real WebSocket.
func (d *DiscordListener) SetOpenGateway(fn func() error) {
	d.openGateway = fn
}

// SetEndpointURL overrides the Discord REST API base URL. Call after
// Connect but before using the session. Used in integration tests with
// a mock HTTP server. The url should end with "/" (e.g. "http://localhost:1234/").
func (d *DiscordListener) SetEndpointURL(url string) {
	if d.session == nil {
		return
	}
	api := url + "api/v" + discordgo.APIVersion + "/"
	discordgo.EndpointDiscord = url
	discordgo.EndpointAPI = api
	discordgo.EndpointGuilds = api + "guilds/"
	discordgo.EndpointChannels = api + "channels/"
	discordgo.EndpointUsers = api + "users/"
	discordgo.EndpointGateway = api + "gateway"
	discordgo.EndpointWebhooks = api + "webhooks/"
}

// Connect initializes the Discord session. Call before Listen.
func (d *DiscordListener) Connect(ctx context.Context) error {
	if d.botToken == "" {
		return fmt.Errorf("discord: bot_token is required")
	}

	session, err := discordgo.New("Bot " + d.botToken)
	if err != nil {
		return fmt.Errorf("discord: failed to create session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
	d.session = session
	d.openGateway = session.Open
	d.log.Debug("discord: connected", "channels", len(d.channels))
	return nil
}

// Listen starts the Discord Gateway connection and delivers incoming
// messages to the returned channel.
func (d *DiscordListener) Listen(ctx context.Context) (<-chan IncomingMessage, error) {
	if d.session == nil {
		return nil, fmt.Errorf("discord: not connected (call Connect first)")
	}

	d.msgs = make(chan IncomingMessage, 100)

	d.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if msg, deliver := d.processMessage(m); deliver {
			d.msgs <- msg
		}
	})

	if err := d.openGateway(); err != nil {
		return nil, fmt.Errorf("discord: failed to open gateway: %w", err)
	}

	d.log.Debug("discord: listening via Gateway WebSocket")

	// Close the message channel when the context is cancelled.
	go func() {
		<-ctx.Done()
		close(d.msgs)
	}()

	return d.msgs, nil
}

// processMessage filters and converts a Discord message into an
// IncomingMessage. Returns the message and true if it should be
// delivered, or false if it should be skipped.
func (d *DiscordListener) processMessage(m *discordgo.MessageCreate) (IncomingMessage, bool) {
	// Skip bot messages (including our own).
	if m.Author == nil || m.Author.Bot {
		d.log.Debug("discord: skipping bot message", "channel", m.ChannelID)
		return IncomingMessage{}, false
	}

	channelID := m.ChannelID
	if d.ignore[channelID] {
		d.log.Debug("discord: skipping ignored channel", "channel", channelID)
		return IncomingMessage{}, false
	}
	if len(d.channels) > 0 && !d.channels[channelID] {
		d.log.Debug("discord: skipping unconfigured channel", "channel", channelID)
		return IncomingMessage{}, false
	}

	channelName := d.resolveChannelName(channelID)
	author := d.resolveAuthor(m)

	d.log.Debug("discord: delivering message", "channel", channelName, "author", author)
	return IncomingMessage{
		ID:        m.ID,
		Service:   "discord",
		Channel:   channelName,
		Author:    author,
		Body:      m.Content,
		Timestamp: time.Now(),
	}, true
}

// ProcessMessageCreate is the exported version of processMessage for testing.
func (d *DiscordListener) ProcessMessageCreate(m *discordgo.MessageCreate) (IncomingMessage, bool) {
	return d.processMessage(m)
}

func (d *DiscordListener) resolveChannelName(id string) string {
	if d.session != nil {
		ch, err := d.session.Channel(id)
		if err != nil {
			d.log.Debug("discord: failed to resolve channel name", "channel_id", id, "error", err)
		} else if ch.Name != "" {
			return "#" + ch.Name
		}
	}
	return id
}

func (d *DiscordListener) resolveAuthor(m *discordgo.MessageCreate) string {
	// Try guild member display name first.
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author != nil {
		if m.Author.GlobalName != "" {
			return m.Author.GlobalName
		}
		return m.Author.Username
	}
	return ""
}

// Send posts a message to the given Discord channel.
func (d *DiscordListener) Send(ctx context.Context, msg OutgoingMessage) error {
	if d.session == nil {
		return fmt.Errorf("discord: not connected")
	}
	_, err := d.session.ChannelMessageSend(msg.Channel, msg.Body)
	return err
}

// Close shuts down the Discord Gateway connection.
func (d *DiscordListener) Close() error {
	if d.session != nil {
		return d.session.Close()
	}
	return nil
}
