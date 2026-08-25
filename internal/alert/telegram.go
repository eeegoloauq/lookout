package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eeegoloauq/lookout/internal/config"
	"golang.org/x/net/proxy"
)

// botToken matches Telegram's token shape (digits:alphanum) so an error
// that echoes the request URL cannot leak it into a log line.
var botToken = regexp.MustCompile(`(?i)(bot)\d+:[A-Za-z0-9_-]+`)

const (
	telegramAPI     = "https://api.telegram.org"
	telegramTimeout = 15 * time.Second
	// How much of Telegram's error body we keep. The token is in the URL,
	// not the body, but a proxy or a verbose error can echo the request.
	telegramErrBody = 200
	// The Bot API echoes the whole sent message on success, which runs to
	// kilobytes. The reply has to be read in full before it can be parsed;
	// reading only as much as an error message needs made every successful
	// send look like a failure, and the outbox redelivered it forever.
	telegramMaxBody = 64 << 10
)

// Telegram delivers messages through the Bot API. Token and chat id come
// only from the constructor — never from a config file — so a copied
// YAML cannot leak them.
type Telegram struct {
	token  string
	chatID string
	api    string
	client *http.Client
}

// TelegramFromEnv builds the production notifier. Missing credentials are
// an error: starting a monitor that cannot notify is the silent-failure
// mode this project exists to prevent.
func TelegramFromEnv(proxyURL string) (*Telegram, error) {
	token, ok := os.LookupEnv(config.EnvTelegramToken)
	if !ok || token == "" {
		return nil, fmt.Errorf("%s is not set: the bot token must come from the environment, not the config file", config.EnvTelegramToken)
	}
	chat, ok := os.LookupEnv(config.EnvTelegramChatID)
	if !ok || chat == "" {
		return nil, fmt.Errorf("%s is not set: the chat id must come from the environment, not the config file", config.EnvTelegramChatID)
	}
	return NewTelegram(token, chat, proxyURL)
}

// NewTelegram constructs a notifier. proxyURL is a socks5:// address; empty
// means dial the API directly. Tests override the API base via SetAPI.
func NewTelegram(token, chatID, proxyURL string) (*Telegram, error) {
	if token == "" {
		return nil, fmt.Errorf("telegram bot token is empty")
	}
	if chatID == "" {
		return nil, fmt.Errorf("telegram chat id is empty")
	}
	tr, err := telegramTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return &Telegram{
		token:  token,
		chatID: chatID,
		api:    telegramAPI,
		client: &http.Client{Transport: tr, Timeout: telegramTimeout},
	}, nil
}

// SetAPI points the client at a different Bot API origin. Tests use this
// to avoid the network; production never should.
func (t *Telegram) SetAPI(base string) { t.api = strings.TrimRight(base, "/") }

func telegramTransport(proxyURL string) (*http.Transport, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Inherited HTTP_PROXY would silently reroute Telegram and is not the
	// SOCKS5 path the target network actually needs.
	tr.Proxy = nil
	if strings.TrimSpace(proxyURL) == "" {
		return tr, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("telegram proxy: %w", err)
	}
	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("telegram proxy: %w", err)
	}
	if cd, ok := dialer.(proxy.ContextDialer); ok {
		tr.DialContext = cd.DialContext
	} else {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	}
	return tr, nil
}

type sendMessageRequest struct {
	ChatID                any    `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type sendMessageResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// Notify posts text to sendMessage. parse_mode is deliberately unset:
// Markdown in an arbitrary body sample is what this format exists to avoid.
func (t *Telegram) Notify(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	payload, err := json.Marshal(sendMessageRequest{
		ChatID:                chatIDValue(t.chatID),
		Text:                  text,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return err
	}
	u := t.api + "/bot" + t.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return t.redactErr(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", t.redactErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, telegramMaxBody))
	body = []byte(t.redact(string(body)))

	var api sendMessageResponse
	parsed := json.Unmarshal(body, &api) == nil
	if resp.StatusCode == http.StatusOK && (api.OK || !parsed) {
		// A 200 is the Bot API's only success signal; failures carry a 4xx.
		// An unparsable 200 is therefore still a delivered message, and
		// treating it as a failure would redeliver rather than lose it —
		// the wrong way round, because the reader gets the same alert twice.
		return nil
	}
	desc := strings.TrimSpace(api.Description)
	if desc == "" {
		desc = strings.TrimSpace(string(body))
	}
	if desc == "" {
		desc = resp.Status
	}
	return fmt.Errorf("telegram sendMessage: HTTP %d: %s", resp.StatusCode, clip(desc, telegramErrBody))
}

func chatIDValue(id string) any {
	if i, err := strconv.ParseInt(id, 10, 64); err == nil {
		return i
	}
	return id
}

func (t *Telegram) redact(s string) string {
	if t.token != "" {
		s = strings.ReplaceAll(s, t.token, "[redacted]")
	}
	// Bot tokens have a well-known shape; catch them even if this
	// instance's token was not the one that leaked.
	s = botToken.ReplaceAllString(s, "${1}[redacted]")
	return s
}

func (t *Telegram) redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := t.redact(err.Error())
	if msg == err.Error() {
		return err
	}
	return fmt.Errorf("%s", msg)
}
