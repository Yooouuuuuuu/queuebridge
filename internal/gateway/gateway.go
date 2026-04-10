package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/internal/broker"
	"github.com/gorilla/websocket"
)

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
	mux.HandleFunc("/tts", g.handleTTS)
	mux.HandleFunc("/ws", g.handleWebSocket)
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
		log.Printf("[gateway] listening on %s  (HTTP POST: /tts  WS: /ws  health: /health)", g.addr)
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

// ---- TTS HTTP handler ----

type ttsRequest struct {
	Text string `json:"text"`
}

func (g *Gateway) handleTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ttsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	log.Printf("[gateway/tts] synthesizing: %q", req.Text)

	resultCh := make(chan broker.Result, 1)
	if err := g.broker.Submit(broker.Job{
		Service:  "tts",
		Payload:  &broker.TTSPayload{Text: req.Text},
		ResultCh: resultCh,
	}); err != nil {
		log.Printf("[gateway/tts] submit failed: %v", err)
		http.Error(w, "TTS not available: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	select {
	case res, ok := <-resultCh:
		if !ok || res.ErrCode != 0 {
			msg := res.ErrMsg
			if !ok {
				msg = "no result received"
			}
			log.Printf("[gateway/tts] synthesis failed: %s", msg)
			http.Error(w, "TTS synthesis failed: "+msg, http.StatusBadGateway)
			return
		}
		log.Printf("[gateway/tts] returning %d bytes of audio", len(res.Audio))
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(res.Audio)
	case <-ctx.Done():
		log.Printf("[gateway/tts] request timed out")
		http.Error(w, "TTS synthesis timed out", http.StatusGatewayTimeout)
	}
}

// ---- STT WebSocket handler ----

type wsIncoming struct {
	Type string `json:"type"`
}

type wsOutgoing struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`
	Code  int    `json:"code,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

func (g *Gateway) handleWebSocket(w http.ResponseWriter, r *http.Request) {
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
				log.Printf("[gateway/ws] bad json from %s: %v", remoteAddr, err)
				writeJSON(wsOutgoing{Type: "error", Msg: "bad json"})
				continue
			}

			switch msg.Type {
			case "start":
				if jobActive {
					continue
				}

				audioCh = make(chan []byte, 256)
				resultCh = make(chan broker.Result, 32)

				if err := g.broker.Submit(broker.Job{
					Service:  "stt",
					Payload:  &broker.STTPayload{AudioCh: audioCh},
					ResultCh: resultCh,
				}); err != nil {
					log.Printf("[gateway/ws] Submit: %v", err)
					writeJSON(wsOutgoing{Type: "error", Msg: err.Error()})
					return
				}

				jobActive = true
				log.Printf("[gateway/ws] STT job submitted for %s", remoteAddr)

				go func() {
					for res := range resultCh {
						if res.ErrCode != 0 {
							writeJSON(wsOutgoing{Type: "error", Code: res.ErrCode, Msg: res.ErrMsg})
						} else {
							writeJSON(wsOutgoing{Type: "result", Text: res.Text, Final: res.IsFinal})
						}
					}
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
