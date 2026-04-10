// sttdebug connects directly to the STT backend (no gateway) and prints
// every raw message received. Useful for diagnosing auth/protocol issues.
//
// Usage:
//
//	STT_TOKEN=xxx go run ./cmd/sttdebug [wavfile]
//	STT_TOKEN=xxx go run ./cmd/sttdebug testdata/input/sample.wav
//
// If no wav file is given it sends 3 seconds of silence.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/config"
	"github.com/gorilla/websocket"
)

const (
	audioType = "audio/L16; rate=16000"
	chunkSize = 4096
	chunkMs   = 128 // ms per 4096-byte chunk at 16kHz mono 16-bit
)

func main() {
	cfg := config.Load()
	token := cfg.STT.Token
	uid := cfg.STT.UID
	endpoint := cfg.STT.Endpoint
	domain := cfg.STT.Domain

	log.Printf("connecting to %s", endpoint)
	log.Printf("token: %q  uid: %q  domain: %q", token, uid, domain)

	conn, resp, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		log.Fatalf("dial failed: %v (http status: %v)", err, resp)
	}
	defer conn.Close()
	log.Println("connected")

	// ---- read handshake ----
	_, raw, err := conn.ReadMessage()
	if err != nil {
		log.Fatalf("read handshake: %v", err)
	}
	log.Printf("← handshake: %s", raw)

	// ---- send StartPayload ----
	start, _ := json.Marshal(map[string]any{
		"action":   "start",
		"domain":   domain,
		"platform": "web",
		"uid":      uid,
		"type":     audioType,
		"bIsDoEPD": true,
		"token":    token,
	})
	log.Printf("→ start: %s", start)
	conn.WriteMessage(websocket.TextMessage, start)

	// ---- load or generate audio ----
	var pcm []byte
	if len(os.Args) > 1 {
		var err error
		pcm, err = readWAVPCM(os.Args[1])
		if err != nil {
			log.Fatalf("read wav: %v", err)
		}
		log.Printf("loaded %d bytes PCM from %s", len(pcm), os.Args[1])
	} else {
		// 3 seconds of silence at 16kHz mono 16-bit
		pcm = make([]byte, 16000*2*3)
		log.Printf("using %d bytes of silence (3s)", len(pcm))
	}

	// ---- stream audio with real-time pacing ----
	log.Println("sending audio...")
	for i := 0; i < len(pcm); i += chunkSize {
		end := i + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			log.Fatalf("write audio: %v", err)
		}
		time.Sleep(chunkMs * time.Millisecond)
	}
	log.Println("audio sent")

	// ---- send StopPayload ----
	stop, _ := json.Marshal(map[string]string{"action": "stop"})
	log.Printf("→ stop: %s", stop)
	conn.WriteMessage(websocket.TextMessage, stop)

	// ---- drain all responses for 10 seconds ----
	log.Println("waiting for responses (10s)...")
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("read ended: %v", err)
			break
		}
		switch msgType {
		case websocket.TextMessage:
			log.Printf("← text: %s", raw)
		case websocket.BinaryMessage:
			log.Printf("← binary: %d bytes", len(raw))
		case websocket.CloseMessage:
			log.Printf("← close: %s", raw)
		}
	}

	log.Println("done")
}

// readWAVPCM extracts raw PCM bytes from a WAV file.
func readWAVPCM(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var riff [4]byte
	io.ReadFull(f, riff[:])
	if string(riff[:]) != "RIFF" {
		return nil, fmt.Errorf("not a RIFF file")
	}
	var fileSize uint32
	binary.Read(f, binary.LittleEndian, &fileSize)
	var wave [4]byte
	io.ReadFull(f, wave[:])
	if string(wave[:]) != "WAVE" {
		return nil, fmt.Errorf("not a WAVE file")
	}

	for {
		var id [4]byte
		if _, err := io.ReadFull(f, id[:]); err != nil {
			return nil, fmt.Errorf("no data chunk found")
		}
		var size uint32
		binary.Read(f, binary.LittleEndian, &size)
		if string(id[:]) == "data" {
			pcm := make([]byte, size)
			io.ReadFull(f, pcm)
			return pcm, nil
		}
		skip := int64(size)
		if size%2 != 0 {
			skip++
		}
		f.Seek(skip, io.SeekCurrent)
	}
}
