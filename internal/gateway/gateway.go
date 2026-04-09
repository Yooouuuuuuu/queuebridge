package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Yooouuuuuuu/queuebridge/config"
	"github.com/Yooouuuuuuu/queuebridge/internal/stt"
	"github.com/Yooouuuuuuu/queuebridge/internal/tts"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Gateway handles inbound HTTP and WebSocket connections.
type Gateway struct {
	addr   string
	cfg    config.Config
	server *http.Server
}

// New creates a new Gateway listening on addr (e.g. ":8080").
func New(addr string, cfg config.Config) *Gateway {
	return &Gateway{addr: addr, cfg: cfg}
}

// Start registers handlers and starts the HTTP server. It blocks until ctx is cancelled.
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

	client := tts.NewClient(tts.Config{
		Endpoint:    g.cfg.TTS.Endpoint,
		Token:       g.cfg.TTS.Token,
		UID:         g.cfg.TTS.UID,
		ServiceName: g.cfg.TTS.ServiceName,
		Speaker:     g.cfg.TTS.Speaker,
		Language:    g.cfg.TTS.Language,
		OutFormat:   g.cfg.TTS.OutFormat,
		VBRQuality:  g.cfg.TTS.VBRQuality,
		Speed:       g.cfg.TTS.Speed,
		Gain:        g.cfg.TTS.Gain,
	})

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		log.Printf("[gateway/tts] connect failed: %v", err)
		http.Error(w, "TTS connect failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer client.Close()

	audioData, err := client.Synthesize(ctx, req.Text)
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Connect to STT backend
	sttClient := stt.NewClient(stt.Config{
		Endpoint: g.cfg.STT.Endpoint,
		Token:    g.cfg.STT.Token,
		UID:      g.cfg.STT.UID,
		Domain:   g.cfg.STT.Domain,
	})

	if err := sttClient.Connect(ctx); err != nil {
		log.Printf("[gateway/ws] STT connect failed: %v", err)
		conn.WriteJSON(wsOutgoing{Type: "error", Msg: "STT connect failed: " + err.Error()})
		return
	}
	defer sttClient.Close()

	// Mutex to protect conn.WriteJSON from concurrent callers (read loop + callbacks)
	var wmu sync.Mutex
	writeJSON := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		conn.WriteJSON(v)
	}

	// Wire STT result/error callbacks → WebSocket client
	sttClient.OnResult(func(text string, isFinal bool) {
		log.Printf("[gateway/ws] STT result: %q final=%v", text, isFinal)
		writeJSON(wsOutgoing{Type: "result", Text: text, Final: isFinal})
	})
	sttClient.OnError(func(code int, msg string) {
		log.Printf("[gateway/ws] STT error: code=%d msg=%s", code, msg)
		writeJSON(wsOutgoing{Type: "error", Code: code, Msg: msg})
	})

	// Pump STT responses → WS client in background
	go func() {
		if err := sttClient.ReadMessages(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[gateway/ws] STT read error: %v", err)
		}
	}()

	// Send welcome
	writeJSON(wsOutgoing{Type: "connected"})

	// Read loop: WS client → STT backend
	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[gateway/ws] client disconnected: %s", remoteAddr)
			} else {
				log.Printf("[gateway/ws] read error from %s: %v", remoteAddr, err)
			}
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
				log.Printf("[gateway/ws] start recognition for %s", remoteAddr)
				if err := sttClient.StartRecognition(); err != nil {
					log.Printf("[gateway/ws] StartRecognition error: %v", err)
					writeJSON(wsOutgoing{Type: "error", Msg: err.Error()})
				}
			case "stop":
				log.Printf("[gateway/ws] stop recognition for %s", remoteAddr)
				if err := sttClient.StopRecognition(); err != nil {
					log.Printf("[gateway/ws] StopRecognition error: %v", err)
					writeJSON(wsOutgoing{Type: "error", Msg: err.Error()})
				}
			default:
				log.Printf("[gateway/ws] unknown type %q from %s", msg.Type, remoteAddr)
			}

		case websocket.BinaryMessage:
			if err := sttClient.SendAudioChunk(raw); err != nil {
				log.Printf("[gateway/ws] SendAudioChunk error: %v", err)
				writeJSON(wsOutgoing{Type: "error", Msg: err.Error()})
			}
		}
	}
}
