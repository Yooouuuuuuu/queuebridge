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
	"strings"
	"sync"
	"sync/atomic"
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

// ---- Session registry ----

type session struct {
	id         string
	clientType string
	stickyPool string
	lastSeen   time.Time
}

type sessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*session
}

func (r *sessionRegistry) create(clientType string) *session {
	s := &session{
		id:         fmt.Sprintf("%x", time.Now().UnixNano()),
		clientType: clientType,
		lastSeen:   time.Now(),
	}
	r.mu.Lock()
	r.sessions[s.id] = s
	r.mu.Unlock()
	log.Printf("[gateway/session] created id=%s type=%s", s.id, s.clientType)
	return s
}

func (r *sessionRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
	log.Printf("[gateway/session] removed id=%s", id)
}

// ---- Gateway ----

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Gateway handles inbound HTTP and WebSocket connections.
type Gateway struct {
	addr     string
	broker   *broker.Broker
	server   *http.Server
	sessions sessionRegistry
}

// New creates a Gateway listening on addr (e.g. ":8080").
func New(addr string, b *broker.Broker) *Gateway {
	return &Gateway{
		addr:     addr,
		broker:   b,
		sessions: sessionRegistry{sessions: make(map[string]*session)},
	}
}

// Start registers handlers and starts the HTTP server. Blocks until ctx is cancelled.
func (g *Gateway) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/http", g.handleHTTP)
	mux.HandleFunc("/v1/ws", g.handleWS)
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
		log.Printf("[gateway] routes: POST /v1/http  WS /v1/ws  GET /health")
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

// ---- POST /v1/http ----

type httpJSONRequest struct {
	Service     string  `json:"service"`
	Pool        string  `json:"pool"`
	Priority    int     `json:"priority"`
	Text        string  `json:"text"`
	Speaker     string  `json:"speaker"`
	Language    string  `json:"language"`
	Speed       float32 `json:"speed"`
	Gain        float32 `json:"gain"`
	OutFormat   string  `json:"out_format"`
	PhraseBreak bool    `json:"phrase_break"`
}

type httpSTTResponse struct {
	Transcript string `json:"transcript"`
	PoolUsed   string `json:"pool_used"`
	Warning    string `json:"warning,omitempty"`
}

func (g *Gateway) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errCodeBadRequest, "method not allowed")
		return
	}

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		var req httpJSONRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeBadRequest, "invalid JSON: "+err.Error())
			return
		}
		switch req.Service {
		case "tts":
			g.handleHTTPTTS(w, r, req)
		default:
			writeError(w, http.StatusServiceUnavailable, errCodeServiceUnavailable,
				fmt.Sprintf("service %q not configured", req.Service))
		}

	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, errCodeBadRequest, "invalid multipart form: "+err.Error())
			return
		}
		switch r.FormValue("service") {
		case "stt":
			g.handleHTTPSTT(w, r)
		default:
			writeError(w, http.StatusServiceUnavailable, errCodeServiceUnavailable,
				fmt.Sprintf("service %q not configured", r.FormValue("service")))
		}

	default:
		writeError(w, http.StatusBadRequest, errCodeBadRequest,
			"Content-Type must be application/json or multipart/form-data")
	}
}

func (g *Gateway) handleHTTPTTS(w http.ResponseWriter, r *http.Request, req httpJSONRequest) {
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

	log.Printf("[gateway/http/tts] pool=%s%s: %q", sr.Pool, warningLabel(sr.Warning), req.Text)

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

func (g *Gateway) handleHTTPSTT(w http.ResponseWriter, r *http.Request) {
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

	log.Printf("[gateway/http/stt] pool=%s%s (%d bytes)", sr.Pool, warningLabel(sr.Warning), len(buf))

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// Feed audio after the session picks up the job (backpressure).
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
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(httpSTTResponse{
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

// ---- WS /v1/ws ----

type wsIncoming struct {
	Type        string  `json:"type"`
	Service     string  `json:"service"`
	SessionType string  `json:"session_type"`
	Pool        string  `json:"pool"`
	Priority    int     `json:"priority"`
	// TTS-specific (used when service == "tts")
	Text        string  `json:"text,omitempty"`
	Speaker     string  `json:"speaker,omitempty"`
	Language    string  `json:"language,omitempty"`
	Speed       float32 `json:"speed,omitempty"`
	Gain        float32 `json:"gain,omitempty"`
	OutFormat   string  `json:"out_format,omitempty"`
	PhraseBreak bool    `json:"phrase_break,omitempty"`
}

type wsOutgoing struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`
	Code  string `json:"code,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

func (g *Gateway) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[gateway/ws] upgrade error: %v", err)
		return
	}
	defer conn.Close()

	remoteAddr := r.RemoteAddr
	log.Printf("[gateway/ws] connected: %s", remoteAddr)

	var wmu sync.Mutex
	writeJSON := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		conn.WriteJSON(v)
	}

	connCtx, connCancel := context.WithCancel(r.Context())
	defer connCancel()

	var (
		jobActive  atomic.Bool
		curAudioCh chan []byte
		closeAudio func()
		sess       *session
	)

	// Ensure in-flight audioCh is closed if the handler exits while a job is active.
	defer func() {
		if closeAudio != nil {
			closeAudio()
		}
	}()

	writeJSON(wsOutgoing{Type: "connected"})

	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[gateway/ws] disconnected: %s (%v)", remoteAddr, err)
			if sess != nil {
				g.sessions.remove(sess.id)
			}
			return
		}

		switch msgType {
		case websocket.BinaryMessage:
			if jobActive.Load() && curAudioCh != nil {
				select {
				case curAudioCh <- raw:
				default:
					log.Printf("[gateway/ws] audio buffer full for %s, dropping chunk", remoteAddr)
				}
			}

		case websocket.TextMessage:
			var msg wsIncoming
			if err := json.Unmarshal(raw, &msg); err != nil {
				writeJSON(wsOutgoing{Type: "error", Code: errCodeBadRequest, Msg: "bad json"})
				continue
			}

			switch msg.Type {
			case "start":
				if jobActive.Load() {
					continue
				}

				// Create session on first job that declares a session_type.
				if msg.SessionType != "" && sess == nil {
					sess = g.sessions.create(msg.SessionType)
					g.startHeartbeat(connCtx, connCancel, conn, &wmu, sess)
				}

				// Soft pool affinity: use sticky pool if client didn't specify one.
				pool := msg.Pool
				if pool == "" && sess != nil && sess.stickyPool != "" {
					pool = sess.stickyPool
				}

				resultCh := make(chan broker.Result, 32)
				var readyCh chan struct{}
				isSessionOriented := sess != nil

				switch msg.Service {
				case "stt":
					audioCh := make(chan []byte, 256)
					once := &sync.Once{}
					curAudioCh = audioCh
					closeAudio = func() { once.Do(func() { close(audioCh) }) }
					readyCh = make(chan struct{})

					sr, err := g.broker.Submit(broker.Job{
						Service:  "stt",
						Pool:     pool,
						Priority: msg.Priority,
						Payload:  &broker.STTPayload{AudioCh: audioCh},
						ResultCh: resultCh,
						ReadyCh:  readyCh,
					})
					if err != nil {
						g.wsSubmitError(writeJSON, err, isSessionOriented)
						if !isSessionOriented {
							return
						}
						continue
					}

					if sess != nil && sess.stickyPool == "" {
						sess.stickyPool = sr.Pool
					}
					log.Printf("[gateway/ws] stt queued pool=%s%s for %s", sr.Pool, warningLabel(sr.Warning), remoteAddr)
					g.wsPostSubmit(writeJSON, connCtx, conn, &wmu, sr, resultCh, readyCh, isSessionOriented, &jobActive, sess)

				case "tts":
					if msg.Text == "" {
						writeJSON(wsOutgoing{Type: "error", Code: errCodeBadRequest, Msg: "text is required for tts"})
						continue
					}
					curAudioCh = nil
					closeAudio = func() {}

					sr, err := g.broker.Submit(broker.Job{
						Service:  "tts",
						Pool:     pool,
						Priority: msg.Priority,
						Payload: &broker.TTSPayload{
							Text:        msg.Text,
							Speaker:     msg.Speaker,
							Language:    msg.Language,
							OutFormat:   msg.OutFormat,
							Speed:       msg.Speed,
							Gain:        msg.Gain,
							PhraseBreak: msg.PhraseBreak,
						},
						ResultCh: resultCh,
					})
					if err != nil {
						g.wsSubmitError(writeJSON, err, isSessionOriented)
						if !isSessionOriented {
							return
						}
						continue
					}

					if sess != nil && sess.stickyPool == "" {
						sess.stickyPool = sr.Pool
					}
					log.Printf("[gateway/ws] tts queued pool=%s%s for %s", sr.Pool, warningLabel(sr.Warning), remoteAddr)
					g.wsPostSubmit(writeJSON, connCtx, conn, &wmu, sr, resultCh, nil, isSessionOriented, &jobActive, sess)

				default:
					// Unknown service — broker will return ErrServiceNotConfigured.
					curAudioCh = nil
					closeAudio = func() {}
					sr, err := g.broker.Submit(broker.Job{
						Service:  msg.Service,
						Pool:     pool,
						Priority: msg.Priority,
						ResultCh: resultCh,
					})
					if err != nil {
						g.wsSubmitError(writeJSON, err, isSessionOriented)
						if !isSessionOriented {
							return
						}
						continue
					}
					// Should not reach here (broker always errors for unknown services),
					// but close the channel to avoid a goroutine leak.
					close(resultCh)
					_ = sr
				}

				jobActive.Store(true)

			case "stop":
				if jobActive.Load() && closeAudio != nil {
					closeAudio()
				}

			default:
				log.Printf("[gateway/ws] unknown type %q from %s", msg.Type, remoteAddr)
			}
		}
	}
}

// wsSubmitError sends an error frame with the appropriate code.
func (g *Gateway) wsSubmitError(writeJSON func(any), err error, isSessionOriented bool) {
	code := errCodeSubmitFailed
	if errors.Is(err, broker.ErrServiceNotConfigured) {
		code = errCodeServiceUnavailable
	} else if errors.Is(err, broker.ErrDraining) {
		code = errCodeShuttingDown
	}
	writeJSON(wsOutgoing{Type: "error", Code: code, Msg: err.Error()})
}

// wsPostSubmit sends warning/ready frames and starts the result goroutine.
// readyCh is nil for services that don't use it (TTS).
func (g *Gateway) wsPostSubmit(
	writeJSON func(any),
	connCtx context.Context,
	conn *websocket.Conn,
	wmu *sync.Mutex,
	sr broker.SubmitResult,
	resultCh <-chan broker.Result,
	readyCh <-chan struct{},
	isSessionOriented bool,
	jobActive *atomic.Bool,
	sess *session,
) {
	if sr.Warning != "" {
		writeJSON(wsOutgoing{Type: "warning", Msg: sr.Warning})
	}

	if readyCh != nil {
		go func() {
			select {
			case <-readyCh:
				writeJSON(wsOutgoing{Type: "ready"})
			case <-connCtx.Done():
			}
		}()
	}

	go func() {
		for res := range resultCh {
			if res.ErrCode != 0 {
				writeJSON(wsOutgoing{Type: "error", Code: errCodeUpstreamFailed, Msg: res.ErrMsg})
			} else if res.Audio != nil {
				wmu.Lock()
				conn.WriteMessage(websocket.BinaryMessage, res.Audio)
				wmu.Unlock()
			} else {
				writeJSON(wsOutgoing{Type: "result", Text: res.Text, Final: res.IsFinal})
			}
		}

		jobActive.Store(false)
		writeJSON(wsOutgoing{Type: "done"})

		if isSessionOriented {
			if sess != nil {
				sess.lastSeen = time.Now()
			}
		} else {
			// Short-lived: send close frame and close connection.
			wmu.Lock()
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			wmu.Unlock()
			conn.Close()
		}
	}()
}

// startHeartbeat starts a ping loop for session-oriented connections.
// Cancels connCtx if a ping fails.
func (g *Gateway) startHeartbeat(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, wmu *sync.Mutex, sess *session) {
	conn.SetPongHandler(func(string) error {
		sess.lastSeen = time.Now()
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				wmu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				wmu.Unlock()
				if err != nil {
					log.Printf("[gateway/ws] heartbeat failed for session %s: %v", sess.id, err)
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// warningLabel formats a warning for log lines; returns empty string when no warning.
func warningLabel(w string) string {
	if w == "" {
		return ""
	}
	return " [warning: " + w + "]"
}
