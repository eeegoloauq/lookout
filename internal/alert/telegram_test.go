package alert

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eeegoloauq/lookout/internal/config"
)

const testToken = "123456:TESTTOKEN-secret-value"
const testChat = "-1001234567890"

func TestTelegramSendsPlainText(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody []byte
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	tg, err := NewTelegram(testToken, testChat, "")
	if err != nil {
		t.Fatal(err)
	}
	tg.SetAPI(srv.URL)

	if err := tg.Notify(context.Background(), "DOWN Photos\nstatus: want 200, got 500"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(gotPath, "/bot"+testToken+"/sendMessage") {
		t.Errorf("path = %q, want the sendMessage method", gotPath)
	}
	var req sendMessageRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Text != "DOWN Photos\nstatus: want 200, got 500" {
		t.Errorf("text = %q", req.Text)
	}
	if req.DisableWebPagePreview != true {
		t.Error("webpage previews should be off so a body sample cannot expand")
	}
	gotChat, _ := json.Marshal(req.ChatID)
	if string(gotChat) != testChat {
		t.Errorf("chat_id = %s, want %s", gotChat, testChat)
	}
}

func TestTelegramErrorDoesNotContainTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Echo the path so a sloppy error formatter would leak the token.
		w.Write([]byte(`{"ok":false,"description":"Unauthorized at ` + r.URL.Path + `"}`))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tg, err := NewTelegram(testToken, testChat, "")
	if err != nil {
		t.Fatal(err)
	}
	tg.SetAPI(srv.URL)

	err = tg.Notify(context.Background(), "hello")
	if err == nil {
		t.Fatal("want an error from a 401")
	}
	log.Error("delivery failed", "err", err)

	blob := err.Error() + "\n" + buf.String()
	if strings.Contains(blob, testToken) || strings.Contains(blob, "TESTTOKEN-secret-value") {
		t.Errorf("token leaked into logs or errors:\n%s", blob)
	}
	if strings.Contains(blob, "123456:") {
		t.Errorf("token prefix leaked:\n%s", blob)
	}
}

func TestTelegramFromEnvRejectsMissingCredentials(t *testing.T) {
	t.Setenv(config.EnvTelegramToken, "")
	t.Setenv(config.EnvTelegramChatID, "")
	if _, err := TelegramFromEnv(""); err == nil || !strings.Contains(err.Error(), config.EnvTelegramToken) {
		t.Errorf("err = %v, want it to name %s", err, config.EnvTelegramToken)
	}
}

func TestTelegramFromEnvReadsTheEnvironment(t *testing.T) {
	t.Setenv(config.EnvTelegramToken, testToken)
	t.Setenv(config.EnvTelegramChatID, testChat)
	tg, err := TelegramFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if tg.token != testToken || tg.chatID != testChat {
		t.Errorf("got token/chat from somewhere other than the environment")
	}
}

func TestTelegramEmptyTextIsANoop(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	tg, err := NewTelegram(testToken, testChat, "")
	if err != nil {
		t.Fatal(err)
	}
	tg.SetAPI(srv.URL)
	if err := tg.Notify(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Error("empty text still hit the API")
	}
}

// SOCKS5 is how Telegram is reached in the target network. If the client
// ignores the configured proxy it will look fine in tests (direct httptest)
// and fail in production.
func TestTelegramUsesSOCKS5Proxy(t *testing.T) {
	var (
		mu       sync.Mutex
		gotText  string
		connects atomic.Int32
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var req sendMessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotText = req.Text
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(api.Close)

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socksLn.Close() })
	go serveSOCKS5(t, socksLn, &connects)

	tg, err := NewTelegram(testToken, testChat, "socks5://"+socksLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tg.SetAPI(api.URL)

	if err := tg.Notify(context.Background(), "proxied"); err != nil {
		t.Fatalf("Notify via SOCKS5: %v", err)
	}
	if connects.Load() < 1 {
		t.Fatal("SOCKS5 proxy was not used")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotText != "proxied" {
		t.Errorf("text = %q", gotText)
	}
}

// serveSOCKS5 is a no-auth SOCKS5 CONNECT relay, just enough to prove the
// HTTP client actually dials through the configured proxy.
func serveSOCKS5(t *testing.T, ln net.Listener, connects *atomic.Int32) {
	t.Helper()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = socks5Relay(c, connects)
		}(c)
	}
}

func socks5Relay(c net.Conn, connects *atomic.Int32) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return err
	}
	var host string
	switch req[3] {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(c, addr); err != nil {
			return err
		}
		host = net.IP(addr).String()
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(c, lb); err != nil {
			return err
		}
		name := make([]byte, int(lb[0]))
		if _, err := io.ReadFull(c, name); err != nil {
			return err
		}
		host = string(name)
	case 0x04:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(c, addr); err != nil {
			return err
		}
		host = net.IP(addr).String()
	default:
		return io.ErrUnexpectedEOF
	}
	portb := make([]byte, 2)
	if _, err := io.ReadFull(c, portb); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(portb)
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer target.Close()
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	connects.Add(1)
	done := make(chan struct{})
	go func() {
		io.Copy(target, c)
		close(done)
	}()
	io.Copy(c, target)
	<-done
	return nil
}
