package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Yooouuuuuuu/queuebridge/internal/broker"
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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	audioData, err := g.broker.Synthesize(ctx, req.Text)
	if err != nil {
		log.Printf("[gateway/tts] synthesis failed: %v", err)
		http.Error(w, "TTS synthesis failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	log.Printf("[gateway/tts] returning %d bytes of audio", len(audioData))
	w.Header().Set("Content-Type", "audio/wav")
	w.Write(audioData)
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
		resultCh  chan broker.STTResult
		audioOnce sync.Once
		jobActive bool
	)

	// closeAudio closes audioCh exactly once, signalling the broker that the
	// client has finished sending audio.
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
			// 1006 (abnormal closure) just means the client exited without
			// sending a close frame — treat it the same as a clean disconnect.
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
					continue // ignore duplicate start
				}

				audioCh = make(chan []byte, 256)
				resultCh = make(chan broker.STTResult, 32)
				job := broker.STTJob{AudioCh: audioCh, ResultCh: resultCh}

				if err := g.broker.SubmitSTT(job); err != nil {
					log.Printf("[gateway/ws] SubmitSTT: %v", err)
					writeJSON(wsOutgoing{Type: "error", Msg: "server busy, try again later"})
					return // close connection so client fails fast and retries
				}

				jobActive = true
				log.Printf("[gateway/ws] STT session queued for %s", remoteAddr)

				// Forward broker results to this WS client.
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
