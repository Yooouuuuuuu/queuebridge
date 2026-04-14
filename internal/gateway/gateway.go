package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/internal/broker"
	"github.com/gorilla/websocket"
)

// ---- Error codes ----

const (
	errCodeServiceUnavailable = "service_unavailable"
	errCodeBadRequest         = "bad_request"
	errCodeSubmitFailed       = "submit_failed"
	errCodeUpstreamFailed     = "upstream_failed"
	errCodeTimeout            = "timeout"
	errCodeShuttingDown       = "shutting_down"
)

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{Error: msg, Code: code})
}

func classifySubmitError(err error) (status int, code string) {
	switch {
	case errors.Is(err, broker.ErrServiceNotConfigured):
		return http.StatusServiceUnavailable, errCodeServiceUnavailable
	case errors.Is(err, broker.ErrDraining):
		return http.StatusServiceUnavailable, errCodeShuttingDown
	default:
		return http.StatusServiceUnavailable, errCodeSubmitFailed
	}
}

// ---- Gateway ----

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Gateway handles inbound HTTP and WebSocket connections.
type Gateway struct {
	addr   string
	broker *broker.Broker
	server *http.Server
}

// New creates a Gateway listening on addr (e.g. ":8080").
func New(addr string, b *broker.Broker) *Gateway {
	return &Gateway{addr: addr, broker: b}
}

// Start registers handlers and starts the HTTP server. Blocks until ctx is cancelled.
func (g *Gateway) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// v1 API routes.
	mux.HandleFunc("/v1/tts", g.handleV1TTS)
	mux.HandleFunc("/v1/stt", g.handleV1STT)
	mux.HandleFunc("/v1/stt/stream", g.handleV1STTStream)
	// Catch-all for unknown /v1/* services — more specific patterns above take priority.
	mux.HandleFunc("/v1/", g.handleUnknownService)

	// Legacy aliases — kept for backward compatibility.
	mux.HandleFunc("/tts", g.handleV1TTS)
	mux.HandleFunc("/ws", g.handleV1STTStream)

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	g.server = &http.Server{
		Addr:    g.addr,
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[gateway] listening on %s", g.addr)
		log.Printf("[gateway] routes: POST /v1/tts  POST /v1/stt  WS /v1/stt/stream  GET /health")
		if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return g.server.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// ---- POST /v1/tts ----

type v1TTSRequest struct {
	Text        string  `json:"text"`
	Pool        string  `json:"pool"`
	Priority    int     `json:"priority"`
	Speaker     string  `json:"speaker"`
	Language    string  `json:"language"`
	Speed       float32 `json:"speed"`
	Gain        float32 `json:"gain"`
	OutFormat   string  `json:"out_format"`
	PhraseBreak bool    `json:"phrase_break"`
}

func (g *Gateway) handleV1TTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errCodeBadRequest, "method not allowed")
		return
	}

	var req v1TTSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, "text is required")
		return
	}

	resultCh := make(chan broker.Result, 1)
	sr, err := g.broker.Submit(broker.Job{
		Service:  "tts",
		Pool:     req.Pool,
		Priority: req.Priority,
		Payload: &broker.TTSPayload{
			Text:        req.Text,
			Speaker:     req.Speaker,
			Language:    req.Language,
			OutFormat:   req.OutFormat,
			Speed:       req.Speed,
			Gain:        req.Gain,
			PhraseBreak: req.PhraseBreak,
		},
		ResultCh: resultCh,
	})
	if err != nil {
		status, code := classifySubmitError(err)
		writeError(w, status, code, err.Error())
		return
	}

	log.Printf("[gateway/tts] synthesizing pool=%s%s: %q",
		sr.Pool, warningLabel(sr.Warning), req.Text)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	select {
	case res, ok := <-resultCh:
		if !ok || res.ErrCode != 0 {
			msg := res.ErrMsg
			if !ok {
				msg = "no result received"
			}
			writeError(w, http.StatusBadGateway, errCodeUpstreamFailed, msg)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-Pool-Used", sr.Pool)
		if sr.Warning != "" {
			w.Header().Set("X-Warning", sr.Warning)
		}
		w.Write(res.Audio)
	case <-ctx.Done():
		writeError(w, http.StatusGatewayTimeout, errCodeTimeout, "TTS synthesis timed out")
	}
}

// ---- POST /v1/stt ----

type v1STTResponse struct {
	Transcript string `json:"transcript"`
	PoolUsed   string `json:"pool_used"`
	Warning    string `json:"warning,omitempty"`
}

func (g *Gateway) handleV1STT(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errCodeBadRequest, "method not allowed")
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	audioFile, _, err := r.FormFile("audio")
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, "audio field is required")
		return
	}
	defer audioFile.Close()

	buf, err := io.ReadAll(audioFile)
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadRequest, "failed to read audio: "+err.Error())
		return
	}

	pool := r.FormValue("pool")
	priority, _ := strconv.Atoi(r.FormValue("priority"))

	audioCh := make(chan []byte, 64)
	resultCh := make(chan broker.Result, 32)
	readyCh := make(chan struct{})

	sr, err := g.broker.Submit(broker.Job{
		Service:  "stt",
		Pool:     pool,
		Priority: priority,
		Payload:  &broker.STTPayload{AudioCh: audioCh},
		ResultCh: resultCh,
		ReadyCh:  readyCh,
	})
	if err != nil {
		status, code := classifySubmitError(err)
		writeError(w, status, code, err.Error())
		return
	}

	log.Printf("[gateway/stt] job queued pool=%s%s (%d bytes)",
		sr.Pool, warningLabel(sr.Warning), len(buf))

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// Feed audio chunks only after the session picks up the job.
	go func() {
		select {
		case <-readyCh:
		case <-ctx.Done():
			close(audioCh)
			return
		}
		const chunkSize = 4096
		for i := 0; i < len(buf); i += chunkSize {
			end := i + chunkSize
			if end > len(buf) {
				end = len(buf)
			}
			select {
			case audioCh <- buf[i:end]:
			case <-ctx.Done():
				close(audioCh)
				return
			}
		}
		close(audioCh)
	}()

	var transcript string
	for {
		select {
		case res, ok := <-resultCh:
			if !ok {
				// Job complete.
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(v1STTResponse{
					Transcript: transcript,
					PoolUsed:   sr.Pool,
					Warning:    sr.Warning,
				})
				return
			}
			if res.ErrCode != 0 {
				writeError(w, http.StatusBadGateway, errCodeUpstreamFailed, res.ErrMsg)
				return
			}
			if res.IsFinal {
				transcript += res.Text
			}
		case <-ctx.Done():
			writeError(w, http.StatusGatewayTimeout, errCodeTimeout, "STT recognition timed out")
			return
		}
	}
}

// ---- WS /v1/stt/stream ----

type wsIncoming struct {
	Type     string `json:"type"`
	Pool     string `json:"pool"`
	Priority int    `json:"priority"`
}

type wsOutgoing struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`
	Code  string `json:"code,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

func (g *Gateway) handleV1STTStream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[gateway/ws] upgrade error: %v", err)
		return
	}
	defer conn.Close()

	remoteAddr := r.RemoteAddr
	log.Printf("[gateway/ws] client connected: %s", remoteAddr)

	var wmu sync.Mutex
	writeJSON := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		conn.WriteJSON(v)
	}

	writeJSON(wsOutgoing{Type: "connected"})

	connCtx, connCancel := context.WithCancel(r.Context())
	defer connCancel()

	var (
		audioCh   chan []byte
		resultCh  chan broker.Result
		audioOnce sync.Once
		jobActive bool
	)

	closeAudio := func() {
		audioOnce.Do(func() {
			if audioCh != nil {
				close(audioCh)
			}
		})
	}
	defer closeAudio()

	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[gateway/ws] client disconnected: %s (%v)", remoteAddr, err)
			return
		}

		switch msgType {
		case websocket.TextMessage:
			var msg wsIncoming
			if err := json.Unmarshal(raw, &msg); err != nil {
				writeJSON(wsOutgoing{Type: "error", Code: errCodeBadRequest, Msg: "bad json"})
				continue
			}

			switch msg.Type {
			case "start":
				if jobActive {
					continue
				}

				audioCh = make(chan []byte, 256)
				resultCh = make(chan broker.Result, 32)
				readyCh := make(chan struct{})

				sr, err := g.broker.Submit(broker.Job{
					Service:  "stt",
					Pool:     msg.Pool,
					Priority: msg.Priority,
					Payload:  &broker.STTPayload{AudioCh: audioCh},
					ResultCh: resultCh,
					ReadyCh:  readyCh,
				})
				if err != nil {
					code := errCodeSubmitFailed
					if errors.Is(err, broker.ErrServiceNotConfigured) {
						code = errCodeServiceUnavailable
					} else if errors.Is(err, broker.ErrDraining) {
						code = errCodeShuttingDown
					}
					writeJSON(wsOutgoing{Type: "error", Code: code, Msg: err.Error()})
					return
				}

				jobActive = true
				log.Printf("[gateway/ws] STT job queued pool=%s%s for %s",
					sr.Pool, warningLabel(sr.Warning), remoteAddr)

				if sr.Warning != "" {
					writeJSON(wsOutgoing{Type: "warning", Msg: sr.Warning})
				}

				go func() {
					select {
					case <-readyCh:
						writeJSON(wsOutgoing{Type: "ready"})
					case <-connCtx.Done():
					}
				}()

				go func() {
					for res := range resultCh {
						if res.ErrCode != 0 {
							writeJSON(wsOutgoing{Type: "error", Code: errCodeUpstreamFailed, Msg: res.ErrMsg})
						} else {
							writeJSON(wsOutgoing{Type: "result", Text: res.Text, Final: res.IsFinal})
						}
					}
					writeJSON(wsOutgoing{Type: "done"})
				}()

			case "stop":
				if jobActive {
					log.Printf("[gateway/ws] stop from %s", remoteAddr)
					closeAudio()
				}

			default:
				log.Printf("[gateway/ws] unknown type %q from %s", msg.Type, remoteAddr)
			}

		case websocket.BinaryMessage:
			if jobActive {
				select {
				case audioCh <- raw:
				default:
					log.Printf("[gateway/ws] audio buffer full for %s, dropping chunk", remoteAddr)
				}
			}
		}
	}
}

// ---- Unknown service catch-all ----

func (g *Gateway) handleUnknownService(w http.ResponseWriter, r *http.Request) {
	// Extract service name from path: /v1/<service>[/...]
	path := r.URL.Path[len("/v1/"):]
	service := path
	if i := len(path); i == 0 {
		writeError(w, http.StatusNotFound, errCodeBadRequest, "missing service in path")
		return
	}
	// Trim any sub-path (e.g. /v1/llm/stream → "llm")
	for i, c := range path {
		if c == '/' {
			service = path[:i]
			break
		}
	}
	writeError(w, http.StatusServiceUnavailable, errCodeServiceUnavailable,
		fmt.Sprintf("service %q not configured", service))
}

// warningLabel formats a warning for log lines; returns empty string when no warning.
func warningLabel(w string) string {
	if w == "" {
		return ""
	}
	return " [warning: " + w + "]"
}
