// playground is a manual testing tool for the queuebridge gateway.
//
// Usage:
//
//	playground stt <file.wav>       stream one WAV through /ws, print transcript
//	playground stt-batch            run all WAVs in testdata/stt/input/ → testdata/stt/output/*.txt
//	playground tts <"text">         synthesize text via /tts → testdata/tts/output/<timestamp>.wav
//	playground tts-batch            synthesize every line in testdata/tts/input/sentences.txt
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	gatewayAddr  = "localhost:8080"
	chunkSize    = 4096
	sttInputDir  = "testdata/stt/input"
	sttOutputDir = "testdata/stt/output"
	ttsInputFile = "testdata/tts/input/sentences.txt"
	ttsOutputDir = "testdata/tts/output"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "stt":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: playground stt <file.wav>")
			os.Exit(1)
		}
		transcript, err := runSTT(os.Args[2])
		if err != nil {
			log.Fatalf("stt: %v", err)
		}
		fmt.Println(transcript)

	case "stt-batch":
		runSTTBatch()

	case "tts":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: playground tts <text>")
			os.Exit(1)
		}
		runTTS(strings.Join(os.Args[2:], " "))

	case "tts-batch":
		runTTSBatch()

	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  playground stt <file.wav>   stream WAV through STT gateway, print transcript
  playground stt-batch        process all WAVs in testdata/stt/input/
  playground tts <text>       synthesize text, save WAV to testdata/tts/output/
  playground tts-batch        synthesize every line in testdata/tts/input/sentences.txt`)
	os.Exit(1)
}

// ---- STT ----------------------------------------------------------------

type wsMsg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`
	Code  int    `json:"code,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

func runSTT(wavPath string) (string, error) {
	hdr, pcm, err := readWAV(wavPath)
	if err != nil {
		return "", fmt.Errorf("read wav: %w", err)
	}

	fmt.Printf("  wav: %d Hz, %d ch, %d-bit, %.2fs\n",
		hdr.SampleRate, hdr.Channels, hdr.BitsPerSample,
		float64(len(pcm))/float64(hdr.ByteRate))

	if hdr.SampleRate != 16000 || hdr.Channels != 1 || hdr.BitsPerSample != 16 {
		fmt.Printf("  WARNING: STT expects 16kHz mono 16-bit — got %dHz %dch %d-bit\n",
			hdr.SampleRate, hdr.Channels, hdr.BitsPerSample)
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+gatewayAddr+"/ws", nil)
	if err != nil {
		return "", fmt.Errorf("ws connect: %w", err)
	}
	defer conn.Close()

	var msg wsMsg
	if err := conn.ReadJSON(&msg); err != nil {
		return "", fmt.Errorf("read welcome: %w", err)
	}
	if msg.Type != "connected" {
		return "", fmt.Errorf("unexpected welcome: %+v", msg)
	}

	send(conn, map[string]string{"type": "start"})

	chunkDuration := time.Duration(float64(chunkSize) / float64(hdr.ByteRate) * float64(time.Second))
	for i := 0; i < len(pcm); i += chunkSize {
		end := i + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			return "", fmt.Errorf("send audio: %w", err)
		}
		time.Sleep(chunkDuration)
	}

	send(conn, map[string]string{"type": "stop"})

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var finals []string
	for {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		switch m.Type {
		case "result":
			fmt.Printf("  [%s] %s\n", boolStr(m.Final, "FINAL", "partial"), m.Text)
			if m.Final {
				finals = append(finals, m.Text)
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			}
		case "error":
			fmt.Printf("  [error] code=%d %s\n", m.Code, m.Msg)
		}
	}

	return strings.Join(finals, " "), nil
}

func runSTTBatch() {
	entries, err := os.ReadDir(sttInputDir)
	if err != nil {
		log.Fatalf("read input dir: %v", err)
	}

	var wavs []string
	for _, e := range entries {
		if !e.IsDir() && strings.ToLower(filepath.Ext(e.Name())) == ".wav" {
			wavs = append(wavs, filepath.Join(sttInputDir, e.Name()))
		}
	}

	if len(wavs) == 0 {
		fmt.Printf("no WAV files found in %s\n", sttInputDir)
		return
	}

	fmt.Printf("processing %d WAV file(s)...\n", len(wavs))
	ok, fail := 0, 0

	for i, path := range wavs {
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		outPath := filepath.Join(sttOutputDir, base+".txt")
		fmt.Printf("[%d/%d] %s\n", i+1, len(wavs), filepath.Base(path))

		transcript, err := runSTT(path)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			fail++
			continue
		}

		if err := os.WriteFile(outPath, []byte(transcript+"\n"), 0644); err != nil {
			fmt.Printf("  WRITE ERROR: %v\n", err)
			fail++
			continue
		}

		fmt.Printf("  → %s\n", outPath)
		ok++
	}

	fmt.Printf("\ndone: %d ok, %d failed\n", ok, fail)
}

// ---- TTS ----------------------------------------------------------------

func runTTS(text string) {
	body, _ := json.Marshal(map[string]string{"text": text})
	resp, err := http.Post("http://"+gatewayAddr+"/tts", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("tts request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		log.Fatalf("tts error %d: %s", resp.StatusCode, msg)
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read response: %v", err)
	}

	outPath := filepath.Join(ttsOutputDir, time.Now().Format("20060102_150405")+"_tts.wav")
	if err := os.WriteFile(outPath, audio, 0644); err != nil {
		log.Fatalf("write output: %v", err)
	}

	fmt.Printf("saved %d bytes → %s\n", len(audio), outPath)
}

func runTTSBatch() {
	f, err := os.Open(ttsInputFile)
	if err != nil {
		log.Fatalf("open sentences file: %v", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		fmt.Printf("no sentences found in %s\n", ttsInputFile)
		return
	}

	fmt.Printf("synthesizing %d sentence(s)...\n", len(lines))
	ok, fail := 0, 0

	for i, text := range lines {
		outPath := filepath.Join(ttsOutputDir, fmt.Sprintf("%04d.wav", i+1))
		fmt.Printf("[%d/%d] %s\n", i+1, len(lines), text)

		body, _ := json.Marshal(map[string]string{"text": text})
		resp, err := http.Post("http://"+gatewayAddr+"/tts", "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			fail++
			continue
		}

		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Printf("  ERROR %d: %s\n", resp.StatusCode, msg)
			fail++
			continue
		}

		audio, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fmt.Printf("  READ ERROR: %v\n", err)
			fail++
			continue
		}

		if err := os.WriteFile(outPath, audio, 0644); err != nil {
			fmt.Printf("  WRITE ERROR: %v\n", err)
			fail++
			continue
		}

		fmt.Printf("  → %s (%d bytes)\n", outPath, len(audio))
		ok++
	}

	fmt.Printf("\ndone: %d ok, %d failed\n", ok, fail)
}

// ---- WAV parsing --------------------------------------------------------

type wavHeader struct {
	AudioFormat   uint16
	Channels      uint16
	SampleRate    uint32
	ByteRate      uint32
	BlockAlign    uint16
	BitsPerSample uint16
}

func readWAV(path string) (wavHeader, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return wavHeader{}, nil, err
	}
	defer f.Close()

	var riff [4]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return wavHeader{}, nil, fmt.Errorf("read RIFF: %w", err)
	}
	if string(riff[:]) != "RIFF" {
		return wavHeader{}, nil, fmt.Errorf("not a RIFF file")
	}
	var fileSize uint32
	binary.Read(f, binary.LittleEndian, &fileSize)
	var wave [4]byte
	if _, err := io.ReadFull(f, wave[:]); err != nil {
		return wavHeader{}, nil, fmt.Errorf("read WAVE: %w", err)
	}
	if string(wave[:]) != "WAVE" {
		return wavHeader{}, nil, fmt.Errorf("not a WAVE file")
	}

	var hdr wavHeader
	var pcm []byte

	for {
		var id [4]byte
		if _, err := io.ReadFull(f, id[:]); err != nil {
			break
		}
		var size uint32
		binary.Read(f, binary.LittleEndian, &size)

		switch string(id[:]) {
		case "fmt ":
			binary.Read(f, binary.LittleEndian, &hdr.AudioFormat)
			binary.Read(f, binary.LittleEndian, &hdr.Channels)
			binary.Read(f, binary.LittleEndian, &hdr.SampleRate)
			binary.Read(f, binary.LittleEndian, &hdr.ByteRate)
			binary.Read(f, binary.LittleEndian, &hdr.BlockAlign)
			binary.Read(f, binary.LittleEndian, &hdr.BitsPerSample)
			if extra := int64(size) - 16; extra > 0 {
				f.Seek(extra, io.SeekCurrent)
			}
		case "data":
			pcm = make([]byte, size)
			io.ReadFull(f, pcm)
		default:
			skip := int64(size)
			if size%2 != 0 {
				skip++
			}
			f.Seek(skip, io.SeekCurrent)
		}

		if pcm != nil {
			break
		}
	}

	if pcm == nil {
		return hdr, nil, fmt.Errorf("no data chunk found")
	}
	return hdr, pcm, nil
}

// ---- helpers ------------------------------------------------------------

func send(conn *websocket.Conn, v any) {
	raw, _ := json.Marshal(v)
	conn.WriteMessage(websocket.TextMessage, raw)
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}
