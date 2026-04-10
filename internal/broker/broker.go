// Package broker holds persistent backend connections and serialises STT
// sessions through a single shared WebSocket connection.
package broker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Yooouuuuuuu/queuebridge/config"
	"github.com/Yooouuuuuuu/queuebridge/internal/stt"
	"github.com/Yooouuuuuuu/queuebridge/internal/tts"
)

const jobQueueCap = 8

// STTJob is one STT session submitted by a gateway client.
type STTJob struct {
	AudioCh  <-chan []byte    // closed by caller when no more audio
	ResultCh chan<- STTResult // closed by broker when the session ends
}

// STTResult is one message the broker delivers back to the gateway client.
type STTResult struct {
	Text    string
	IsFinal bool
	ErrCode int    // non-zero means error
	ErrMsg  string
}

// Broker holds persistent backend connections and routes requests through them.
type Broker struct {
	cfg      config.Config
	sttConns int // number of persistent STT connections (0 = disabled)
	ttsConns int // number of persistent TTS connections (0 = disabled)

	// TTS pool: round-robin across connections.
	ttsClients []*tts.Client
	ttsIdx     atomic.Int64 // round-robin counter, safe for concurrent use

	jobCh          chan STTJob
	activeSessions atomic.Int32 // currently running STT sessions
}

// New creates a Broker with the given pool sizes. Call Start to connect.
// Passing 0 for either pool disables that service.
func New(cfg config.Config, sttConns, ttsConns int) *Broker {
	// Queue must hold at least as many jobs as there are workers, so all
	// workers can receive a job simultaneously without blocking submitters.
	qCap := jobQueueCap
	if sttConns > qCap {
		qCap = sttConns
	}
	return &Broker{
		cfg:      cfg,
		sttConns: sttConns,
		ttsConns: ttsConns,
		jobCh:    make(chan STTJob, qCap),
	}
}

// Start connects to backends and launches background workers.
// It returns once all connections are established and goroutines running.
func (b *Broker) Start(ctx context.Context) error {
	// --- TTS pool (gRPC) ---
	for i := range b.ttsConns {
		cli := tts.NewClient(tts.Config{
			Endpoint:    b.cfg.TTS.Endpoint,
			Token:       b.cfg.TTS.Token,
			UID:         b.cfg.TTS.UID,
			ServiceName: b.cfg.TTS.ServiceName,
			Speaker:     b.cfg.TTS.Speaker,
			Language:    b.cfg.TTS.Language,
			OutFormat:   b.cfg.TTS.OutFormat,
			VBRQuality:  b.cfg.TTS.VBRQuality,
			Speed:       b.cfg.TTS.Speed,
			Gain:        b.cfg.TTS.Gain,
		})
		ttsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := cli.Connect(ttsCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("TTS[%d] connect: %w", i, err)
		}
		b.ttsClients = append(b.ttsClients, cli)
		log.Printf("[broker] TTS[%d] connected", i)
	}

	// --- STT pool (WebSocket) ---
	// All workers share the same jobCh; Go's channel receive is naturally
	// load-balanced across goroutines, so this gives us a free worker pool.
	for i := range b.sttConns {
		cli, err := b.dialSTT(ctx)
		if err != nil {
			return err
		}
		log.Printf("[broker] STT[%d] connected", i)
			go b.runSTTWorker(ctx, cli)
	}

	go b.runStatusLogger(ctx)
	return nil
}

// dialSTT creates and connects an STT client, retrying until ctx is cancelled.
func (b *Broker) dialSTT(ctx context.Context) (*stt.Client, error) {
	for {
		cli := stt.NewClient(stt.Config{
			Endpoint: b.cfg.STT.Endpoint,
			Token:    b.cfg.STT.Token,
			UID:      b.cfg.STT.UID,
			Domain:   b.cfg.STT.Domain,
		})
		if err := cli.Connect(ctx); err == nil {
			return cli, nil
		} else {
			log.Printf("[broker] STT connect failed: %v, retrying in 5s", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// runSTTWorker serialises STT sessions through a persistent WebSocket
// connection, reconnecting automatically when the connection drops.
func (b *Broker) runSTTWorker(ctx context.Context, cli *stt.Client) {
	defer cli.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case job := <-b.jobCh:
			dropped := b.processSTTSession(ctx, cli, job)
			if dropped {
				cli.Close()
				var err error
				cli, err = b.dialSTT(ctx)
				if err != nil {
					return // ctx cancelled
				}
				log.Println("[broker] STT reconnected")
			}
		}
	}
}

// runStatusLogger prints pool status every 10 seconds.
func (b *Broker) runStatusLogger(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("[broker] status — STT workers: %d  active: %d  queued: %d  TTS conns: %d",
				b.sttConns, b.activeSessions.Load(), len(b.jobCh), b.ttsConns)
		}
	}
}

// processSTTSession runs one complete STT session.
// It returns true if the backend connection was lost during the session.
func (b *Broker) processSTTSession(ctx context.Context, cli *stt.Client, job STTJob) (connDropped bool) {
	b.activeSessions.Add(1)
	defer b.activeSessions.Add(-1)
	sessionDone := make(chan struct{})
	var once sync.Once
	signalDone := func() { once.Do(func() { close(sessionDone) }) }

	// Wire callbacks for this session only.
	cli.OnResult(func(text string, isFinal bool) {
		select {
		case job.ResultCh <- STTResult{Text: text, IsFinal: isFinal}:
		case <-sessionDone:
		}
		if isFinal {
			signalDone()
		}
	})
	cli.OnError(func(code int, msg string) {
		select {
		case job.ResultCh <- STTResult{ErrCode: code, ErrMsg: msg}:
		case <-sessionDone:
		}
	})

	// Read backend messages for this session.
	readCtx, cancelRead := context.WithCancel(ctx)
	connErrCh := make(chan error, 1)
	go func() {
		if err := cli.ReadMessages(readCtx); readCtx.Err() == nil && err != nil {
			connErrCh <- err
			signalDone()
		}
	}()

	if err := cli.StartRecognition(); err != nil {
		log.Printf("[broker] StartRecognition: %v", err)
		cancelRead()
		close(job.ResultCh)
		return true // treat as dropped; worker will reconnect
	}

	// Drain audio gateway → backend; stop recognition when audioCh is closed.
	go func() {
		for chunk := range job.AudioCh {
			if err := cli.SendAudioChunk(chunk); err != nil {
				log.Printf("[broker] SendAudioChunk: %v", err)
				break
			}
		}
		if err := cli.StopRecognition(); err != nil {
			log.Printf("[broker] StopRecognition: %v", err)
		}
	}()

	// Wait for session end: final result, timeout, connection drop, or shutdown.
	select {
	case <-sessionDone:
	case <-time.After(30 * time.Second):
		log.Printf("[broker] STT session timed out waiting for final result")
	case <-ctx.Done():
		cancelRead()
		close(job.ResultCh)
		return false
	}

	cancelRead()

	select {
	case err := <-connErrCh:
		log.Printf("[broker] STT connection dropped during session: %v", err)
		close(job.ResultCh)
		return true
	default:
		close(job.ResultCh)
		return false
	}
}

// SubmitSTT queues an STT session. Returns an error if the queue is full or
// if no STT connections were configured.
func (b *Broker) SubmitSTT(job STTJob) error {
	if b.sttConns == 0 {
		return fmt.Errorf("STT not configured (start server with --stt N)")
	}
	select {
	case b.jobCh <- job:
		return nil
	default:
		return fmt.Errorf("STT queue full (%d slots)", jobQueueCap)
	}
}

// Synthesize converts text to speech, round-robining across the TTS pool.
// Safe to call concurrently.
func (b *Broker) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if len(b.ttsClients) == 0 {
		return nil, fmt.Errorf("TTS not configured (start server with --tts N)")
	}
	idx := b.ttsIdx.Add(1) % int64(len(b.ttsClients))
	return b.ttsClients[idx].Synthesize(ctx, text)
}
