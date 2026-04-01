package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultChunkSize = 4096
	DefaultDomain    = "freeSTT-zh-TW"
	DefaultAudioType = "audio/L16; rate=16000"
)

// StartPayload is sent to begin recognition
type StartPayload struct {
	Domain   string `json:"domain"`
	Type     string `json:"type"`
	BIsDoEPD bool   `json:"bIsDoEPD"`
	Token    string `json:"token"`
	UID      string `json:"uid"`
}

// StopPayload is sent to end recognition
type StopPayload struct {
	Status string `json:"status"`
}

// ServerMessage represents messages from the STT server
type ServerMessage struct {
	State   string `json:"state,omitempty"`
	Text    string `json:"text,omitempty"`
	Final   bool   `json:"final,omitempty"`
	ErrCode int    `json:"err_code,omitempty"`
	ErrMsg  string `json:"err_msg,omitempty"`
}

// ResultHandler is called when recognition results are received
type ResultHandler func(text string, isFinal bool)

// ErrorHandler is called when errors occur
type ErrorHandler func(code int, msg string)

// Client is the STT WebSocket client
type Client struct {
	endpoint string
	token    string
	uid      string
	domain   string

	conn          *websocket.Conn
	mu            sync.Mutex
	connected     bool
	listening     bool
	onResult      ResultHandler
	onError       ErrorHandler
}

// Config holds STT client configuration
type Config struct {
	Endpoint string
	Token    string
	UID      string
	Domain   string
}

// NewClient creates a new STT client
func NewClient(cfg Config) *Client {
	domain := cfg.Domain
	if domain == "" {
		domain = DefaultDomain
	}

	return &Client{
		endpoint: cfg.Endpoint,
		token:    cfg.Token,
		uid:      cfg.UID,
		domain:   domain,
	}
}

// OnResult sets the handler for recognition results
func (c *Client) OnResult(handler ResultHandler) {
	c.onResult = handler
}

// OnError sets the handler for errors
func (c *Client) OnError(handler ErrorHandler) {
	c.onError = handler
}

// Connect establishes the WebSocket connection and waits for handshake
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return fmt.Errorf("already connected")
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	c.conn = conn

	// Wait for handshake - server sends {"state": "listening"}
	_, msgBytes, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to read handshake: %w", err)
	}

	var msg ServerMessage
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		conn.Close()
		return fmt.Errorf("failed to parse handshake: %w", err)
	}

	if msg.State != "listening" {
		conn.Close()
		return fmt.Errorf("unexpected handshake state: %s", msg.State)
	}

	c.connected = true
	c.listening = true
	return nil
}

// StartRecognition sends the start payload to begin recognition
func (c *Client) StartRecognition() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	payload := StartPayload{
		Domain:   c.domain,
		Type:     DefaultAudioType,
		BIsDoEPD: true,
		Token:    c.token,
		UID:      c.uid,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal start payload: %w", err)
	}

	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to send start payload: %w", err)
	}

	return nil
}

// SendAudio sends raw PCM audio data (16-bit, 16kHz)
func (c *Client) SendAudio(audioData []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	// Send in chunks
	for i := 0; i < len(audioData); i += DefaultChunkSize {
		end := i + DefaultChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}
		chunk := audioData[i:end]

		if err := c.conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			return fmt.Errorf("failed to send audio chunk: %w", err)
		}
	}

	return nil
}

// SendAudioChunk sends a single chunk of audio data
func (c *Client) SendAudioChunk(chunk []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	if err := c.conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
		return fmt.Errorf("failed to send audio chunk: %w", err)
	}

	return nil
}

// StopRecognition sends the stop signal
func (c *Client) StopRecognition() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	payload := StopPayload{Status: "done"}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal stop payload: %w", err)
	}

	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("failed to send stop payload: %w", err)
	}

	return nil
}

// ReadMessages reads messages from the server in a loop
// This should be called in a goroutine after Connect()
func (c *Client) ReadMessages(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, msgBytes, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return fmt.Errorf("read error: %w", err)
		}

		var msg ServerMessage
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue // Skip malformed messages
		}

		// Handle different message types
		if msg.ErrCode != 0 || msg.ErrMsg != "" {
			if c.onError != nil {
				c.onError(msg.ErrCode, msg.ErrMsg)
			}
			continue
		}

		if msg.State == "listening" {
			c.mu.Lock()
			c.listening = true
			c.mu.Unlock()
			continue
		}

		if msg.Text != "" {
			if c.onResult != nil {
				c.onResult(msg.Text, msg.Final)
			}
		}
	}
}

// Close closes the WebSocket connection
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil
	}

	c.connected = false
	c.listening = false

	if c.conn != nil {
		// Send close message
		c.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		return c.conn.Close()
	}

	return nil
}

// IsConnected returns the connection status
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// IsListening returns whether server is in listening state
func (c *Client) IsListening() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listening
}
