package tts

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"

	pb "github.com/Yooouuuuuuu/queuebridge/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	DefaultServiceName = "e2e"
	DefaultOutFormat   = "wav"
	DefaultVBRQuality  = 4
	DefaultLanguage    = "zh-TW"
	DefaultSpeaker     = "Sharon"
	DefaultSpeed       = 1.0
	DefaultGain        = 1.0
)

// Config holds TTS client configuration
type Config struct {
	Endpoint    string
	Token       string
	UID         string
	ServiceName string
	Speaker     string
	Language    string
	OutFormat   string
	VBRQuality  int64
	Speed       float32
	Gain        float32
}

// Client is the TTS gRPC client
type Client struct {
	endpoint    string
	token       string
	uid         string
	serviceName string
	speaker     string
	language    string
	outFormat   string
	vbrQuality  int64
	speed       float32
	gain        float32

	conn   *grpc.ClientConn
	client pb.StreamServiceClient
}

// NewClient creates a new TTS client
func NewClient(cfg Config) *Client {
	// Apply defaults
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	outFormat := cfg.OutFormat
	if outFormat == "" {
		outFormat = DefaultOutFormat
	}
	vbrQuality := cfg.VBRQuality
	if vbrQuality == 0 {
		vbrQuality = DefaultVBRQuality
	}
	language := cfg.Language
	if language == "" {
		language = DefaultLanguage
	}
	speaker := cfg.Speaker
	if speaker == "" {
		speaker = DefaultSpeaker
	}
	speed := cfg.Speed
	if speed == 0 {
		speed = DefaultSpeed
	}
	gain := cfg.Gain
	if gain == 0 {
		gain = DefaultGain
	}

	return &Client{
		endpoint:    cfg.Endpoint,
		token:       cfg.Token,
		uid:         cfg.UID,
		serviceName: serviceName,
		speaker:     speaker,
		language:    language,
		outFormat:   outFormat,
		vbrQuality:  vbrQuality,
		speed:       speed,
		gain:        gain,
	}
}

// Connect establishes the gRPC connection with TLS (skip verification for self-signed cert)
func (c *Client) Connect(ctx context.Context) error {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	creds := credentials.NewTLS(tlsConfig)

	conn, err := grpc.DialContext(ctx, c.endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("grpc dial failed: %w", err)
	}

	c.conn = conn
	c.client = pb.NewStreamServiceClient(conn)
	return nil
}

// Synthesize converts text to speech and returns the full audio data
func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if c.client == nil {
		return nil, fmt.Errorf("not connected")
	}

	req := &pb.TtsRequest{
		ServiceName: c.serviceName,
		Text:        text,
		Outfmt:      c.outFormat,
		VbrQuality:  c.vbrQuality,
		Language:    c.language,
		Phrbrk:      false,
		Speaker:     c.speaker,
		Speed:       c.speed,
		Gain:        c.gain,
		Token:       c.token,
		Uid:         c.uid,
	}

	stream, err := c.client.TTS(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("TTS call failed: %w", err)
	}

	var audioData []byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream recv error: %w", err)
		}
		audioData = append(audioData, resp.Data...)
	}

	return audioData, nil
}

// SynthesizeStream converts text to speech and streams audio chunks to a channel
func (c *Client) SynthesizeStream(ctx context.Context, text string) (<-chan []byte, <-chan error) {
	dataCh := make(chan []byte, 10)
	errCh := make(chan error, 1)

	go func() {
		defer close(dataCh)
		defer close(errCh)

		if c.client == nil {
			errCh <- fmt.Errorf("not connected")
			return
		}

		req := &pb.TtsRequest{
			ServiceName: c.serviceName,
			Text:        text,
			Outfmt:      c.outFormat,
			VbrQuality:  c.vbrQuality,
			Language:    c.language,
			Phrbrk:      false,
			Speaker:     c.speaker,
			Speed:       c.speed,
			Gain:        c.gain,
			Token:       c.token,
			Uid:         c.uid,
		}

		stream, err := c.client.TTS(ctx, req)
		if err != nil {
			errCh <- fmt.Errorf("TTS call failed: %w", err)
			return
		}

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				errCh <- fmt.Errorf("stream recv error: %w", err)
				return
			}

			select {
			case dataCh <- resp.Data:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	return dataCh, errCh
}

// SynthesizeWithOptions converts text to speech with custom options
func (c *Client) SynthesizeWithOptions(ctx context.Context, text string, opts SynthesizeOptions) ([]byte, error) {
	if c.client == nil {
		return nil, fmt.Errorf("not connected")
	}

	// Use client defaults if options not specified
	speaker := opts.Speaker
	if speaker == "" {
		speaker = c.speaker
	}
	language := opts.Language
	if language == "" {
		language = c.language
	}
	outFormat := opts.OutFormat
	if outFormat == "" {
		outFormat = c.outFormat
	}
	speed := opts.Speed
	if speed == 0 {
		speed = c.speed
	}
	gain := opts.Gain
	if gain == 0 {
		gain = c.gain
	}

	req := &pb.TtsRequest{
		ServiceName: c.serviceName,
		Text:        text,
		Outfmt:      outFormat,
		VbrQuality:  c.vbrQuality,
		Language:    language,
		Phrbrk:      opts.PhraseBreak,
		Speaker:     speaker,
		Speed:       speed,
		Gain:        gain,
		Token:       c.token,
		Uid:         c.uid,
	}

	stream, err := c.client.TTS(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("TTS call failed: %w", err)
	}

	var audioData []byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("stream recv error: %w", err)
		}
		audioData = append(audioData, resp.Data...)
	}

	return audioData, nil
}

// SynthesizeOptions allows overriding default synthesis parameters
type SynthesizeOptions struct {
	Speaker     string
	Language    string
	OutFormat   string
	Speed       float32
	Gain        float32
	PhraseBreak bool
}

// Close closes the gRPC connection
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// IsConnected returns whether the client is connected
func (c *Client) IsConnected() bool {
	return c.conn != nil && c.client != nil
}
