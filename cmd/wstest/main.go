package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

type wsMsg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`
	Code  int    `json:"code,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

func main() {
	conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:8080/ws", nil)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// Read welcome
	var welcome wsMsg
	conn.ReadJSON(&welcome)
	fmt.Printf("← %+v\n", welcome)

	// Start recognition
	send(conn, map[string]string{"type": "start"})

	// Send fake PCM audio chunks (silence: all zeros, 16-bit PCM 16kHz)
	// 10 chunks × 4096 bytes = ~1.28 seconds of audio
	chunk := make([]byte, 4096)
	for i := 0; i < 10; i++ {
		fmt.Printf("→ [binary chunk %d] %d bytes\n", i+1, len(chunk))
		conn.WriteMessage(websocket.BinaryMessage, chunk)
		time.Sleep(80 * time.Millisecond) // pace at roughly real-time
	}

	// Stop recognition
	send(conn, map[string]string{"type": "stop"})

	// Drain responses for 3 seconds
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var msg wsMsg
		json.Unmarshal(raw, &msg)
		fmt.Printf("← %+v\n", msg)
	}

	fmt.Println("done")
}

func send(conn *websocket.Conn, v any) {
	raw, _ := json.Marshal(v)
	fmt.Printf("→ %s\n", raw)
	conn.WriteMessage(websocket.TextMessage, raw)
}
