// playground is a manual testing tool for the gateway.
//
// Usage:
//
//	playground stt <file.wav>       stream one WAV through /v1/ws, print transcript
//	playground stt-batch            run all WAVs in testdata/stt/input/ → testdata/stt/output/*.txt
//	playground tts <"text">         synthesize text via /v1/http → testdata/tts/output/<timestamp>.wav
//	playground tts-batch            synthesize every line in testdata/tts/input/sentences.txt
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
		res, err := runSTT(os.Args[2])
		if err != nil {
			log.Fatalf("stt: %v", err)
		}
		fmt.Println(res.transcript)
		outPath := filepath.Join(sttOutputDir, time.Now().Format("20060102_150405")+"_stt.txt")
		if err := os.WriteFile(outPath, []byte(res.transcript+"\n"), 0644); err != nil {
			log.Printf("save transcript: %v", err)
		} else {
			fmt.Printf("saved → %s\n", outPath)
		}

	case "stt-batch":
		fs := flag.NewFlagSet("stt-batch", flag.ExitOnError)
		workers := fs.Int("workers", 1, "concurrent STT sessions")
		fs.Parse(os.Args[2:])
		runSTTBatch(*workers)

	case "tts":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: playground tts <text>")
			os.Exit(1)
		}
		runTTS(strings.Join(os.Args[2:], " "))

	case "tts-batch":
		fs := flag.NewFlagSet("tts-batch", flag.ExitOnError)
		workers := fs.Int("workers", 1, "concurrent TTS requests")
		fs.Parse(os.Args[2:])
		runTTSBatch(*workers)

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
	Code  string `json:"code,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

// sttResult holds transcript and any server error codes seen during the session.
type sttResult struct {
	transcript string
	errCodes   []string // error codes received from server
}

func runSTT(wavPath string) (sttResult, error) {
	hdr, pcm, err := readWAV(wavPath)
	if err != nil {
		return sttResult{}, fmt.Errorf("read wav: %w", err)
	}

	fmt.Printf("  wav: %d Hz, %d ch, %d-bit, %.2fs\n",
		hdr.SampleRate, hdr.Channels, hdr.BitsPerSample,
		float64(len(pcm))/float64(hdr.ByteRate))

	if hdr.SampleRate != 16000 || hdr.Channels != 1 || hdr.BitsPerSample != 16 {
		fmt.Printf("  WARNING: STT expects 16kHz mono 16-bit — got %dHz %dch %d-bit\n",
			hdr.SampleRate, hdr.Channels, hdr.BitsPerSample)
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+gatewayAddr+"/v1/ws", nil)
	if err != nil {
		return sttResult{}, fmt.Errorf("ws connect: %w", err)
	}
	defer conn.Close()

	var msg wsMsg
	if err := conn.ReadJSON(&msg); err != nil {
		return sttResult{}, fmt.Errorf("read welcome: %w", err)
	}
	if msg.Type != "connected" {
		return sttResult{}, fmt.Errorf("unexpected welcome: %+v", msg)
	}

	send(conn, map[string]any{"type": "start", "service": "stt"})

	// Wait for the broker to signal that a session has picked up the job.
	// This prevents streaming audio into a buffer while the job sits in queue —
	// with N connections and many workers, the queue wait can exceed the read deadline.
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	var readyMsg wsMsg
	if err := conn.ReadJSON(&readyMsg); err != nil {
		return sttResult{}, fmt.Errorf("wait ready: %w", err)
	}
	if readyMsg.Type == "error" {
		return sttResult{}, fmt.Errorf("session error while queued: %s", readyMsg.Msg)
	}
	if readyMsg.Type != "ready" {
		return sttResult{}, fmt.Errorf("unexpected message (type=%s) while waiting for ready", readyMsg.Type)
	}
	conn.SetReadDeadline(time.Time{})

	for i := 0; i < len(pcm); i += chunkSize {
		end := i + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			return sttResult{}, fmt.Errorf("send audio: %w", err)
		}
	}

	send(conn, map[string]string{"type": "stop"})

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var res sttResult
	for {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		switch m.Type {
		case "result":
			fmt.Printf("  [%s] %s\n", boolStr(m.Final, "FINAL", "partial"), m.Text)
			if m.Final {
				res.transcript += m.Text + " "
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			}
		case "error":
			fmt.Printf("  [error] code=%s %s\n", m.Code, m.Msg)
			if m.Code != "" {
				res.errCodes = append(res.errCodes, m.Code)
				// Fatal broker/session error — no transcript is coming; fail fast.
				return res, fmt.Errorf("session error %s: %s", m.Code, m.Msg)
			}
		case "done":
			// Broker signals the job is fully complete; no need to wait for deadline.
			res.transcript = strings.TrimSpace(res.transcript)
			if res.transcript == "" {
				return res, fmt.Errorf("no transcript received (session rejected or timed out)")
			}
			return res, nil
		}
	}

	res.transcript = strings.TrimSpace(res.transcript)
	if res.transcript == "" {
		return res, fmt.Errorf("no transcript received (session rejected or timed out)")
	}
	return res, nil
}

func runSTTBatch(workers int) {
	dirEntries, err := os.ReadDir(sttInputDir)
	if err != nil {
		log.Fatalf("read input dir: %v", err)
	}

	var wavs []string
	for _, e := range dirEntries {
		if !e.IsDir() && strings.ToLower(filepath.Ext(e.Name())) == ".wav" {
			wavs = append(wavs, filepath.Join(sttInputDir, e.Name()))
		}
	}

	if len(wavs) == 0 {
		fmt.Printf("no WAV files found in %s\n", sttInputDir)
		return
	}

	if workers < 1 {
		workers = 1
	}
	fmt.Printf("processing %d WAV file(s) with %d concurrent worker(s)...\n", len(wavs), workers)

	batchStart := time.Now()
	results := make([]batchEntry, len(wavs)) // indexed by position, safe without mutex

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, path := range wavs {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			label := filepath.Base(path)
			base := strings.TrimSuffix(label, filepath.Ext(label))
			outPath := filepath.Join(sttOutputDir, base+".txt")

			fmt.Printf("[%d/%d] %s starting\n", i+1, len(wavs), label)
			start := time.Now()

			const maxRetries = 3
			var res sttResult
			var err error
			var retries int
			for attempt := range maxRetries {
				if attempt > 0 {
					retries++
					wait := time.Duration(attempt) * 2 * time.Second
					fmt.Printf("[%d/%d] retry %d/%d in %s\n", i+1, len(wavs), attempt, maxRetries-1, wait)
					time.Sleep(wait)
				}
				res, err = runSTT(path)
				if err == nil {
					break
				}
				fmt.Printf("[%d/%d] attempt %d failed: %v\n", i+1, len(wavs), attempt+1, err)
			}

			end := time.Now()

			if err != nil {
				fmt.Printf("[%d/%d] ERROR (all retries exhausted): %v\n", i+1, len(wavs), err)
				results[i] = batchEntry{i + 1, label, start, end, false, err.Error(), retries, res.errCodes}
				return
			}
			if werr := os.WriteFile(outPath, []byte(res.transcript+"\n"), 0644); werr != nil {
				fmt.Printf("[%d/%d] WRITE ERROR: %v\n", i+1, len(wavs), werr)
				results[i] = batchEntry{i + 1, label, start, end, false, werr.Error(), retries, res.errCodes}
				return
			}
			fmt.Printf("[%d/%d] done  → %s  (%.2fs)\n", i+1, len(wavs), outPath, end.Sub(start).Seconds())
			results[i] = batchEntry{i + 1, label, start, end, true, outPath, retries, res.errCodes}
		}(i, path)
	}

	wg.Wait()

	ok, fail := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			fail++
		}
	}
	fmt.Printf("\ndone: %d ok, %d failed\n", ok, fail)

	logPath := filepath.Join(sttOutputDir, batchStart.Format("20060102_150405")+"_batch.log")
	writeBatchLog(logPath, batchStart, results)
	fmt.Printf("log  → %s\n", logPath)
}

// ---- TTS ----------------------------------------------------------------

func runTTS(text string) {
	body, _ := json.Marshal(map[string]string{"service": "tts", "text": text})
	resp, err := http.Post("http://"+gatewayAddr+"/v1/http", "application/json", bytes.NewReader(body))
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

func runTTSBatch(workers int) {
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

	if workers < 1 {
		workers = 1
	}
	fmt.Printf("synthesizing %d sentence(s) with %d concurrent worker(s)...\n", len(lines), workers)

	batchStart := time.Now()
	entries := make([]batchEntry, len(lines))
	var ok, fail atomic.Int32

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, text := range lines {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, text string) {
			defer wg.Done()
			defer func() { <-sem }()

			outPath := filepath.Join(ttsOutputDir, fmt.Sprintf("%04d.wav", i+1))
			start := time.Now()

			body, _ := json.Marshal(map[string]string{"service": "tts", "text": text})
			resp, err := http.Post("http://"+gatewayAddr+"/v1/http", "application/json", bytes.NewReader(body))
			if err != nil {
				end := time.Now()
				fmt.Printf("[%d/%d] ERROR: %v\n", i+1, len(lines), err)
				entries[i] = batchEntry{Index: i + 1, Label: text, Start: start, End: end, Note: err.Error()}
				fail.Add(1)
				return
			}

			if resp.StatusCode != http.StatusOK {
				msg, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				end := time.Now()
				fmt.Printf("[%d/%d] ERROR %d: %s\n", i+1, len(lines), resp.StatusCode, msg)
				entries[i] = batchEntry{Index: i + 1, Label: text, Start: start, End: end, Note: fmt.Sprintf("HTTP %d", resp.StatusCode)}
				fail.Add(1)
				return
			}

			audio, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				end := time.Now()
				fmt.Printf("[%d/%d] READ ERROR: %v\n", i+1, len(lines), err)
				entries[i] = batchEntry{Index: i + 1, Label: text, Start: start, End: end, Note: err.Error()}
				fail.Add(1)
				return
			}

			if err := os.WriteFile(outPath, audio, 0644); err != nil {
				end := time.Now()
				fmt.Printf("[%d/%d] WRITE ERROR: %v\n", i+1, len(lines), err)
				entries[i] = batchEntry{Index: i + 1, Label: text, Start: start, End: end, Note: err.Error()}
				fail.Add(1)
				return
			}

			end := time.Now()
			fmt.Printf("[%d/%d] → %s (%d bytes)\n", i+1, len(lines), outPath, len(audio))
			entries[i] = batchEntry{Index: i + 1, Label: text, Start: start, End: end, OK: true, Note: outPath}
			ok.Add(1)
		}(i, text)
	}
	wg.Wait()

	ok2, fail2 := int(ok.Load()), int(fail.Load())

	fmt.Printf("\ndone: %d ok, %d failed\n", ok2, fail2)

	logPath := filepath.Join(ttsOutputDir, batchStart.Format("20060102_150405")+"_batch.log")
	writeBatchLog(logPath, batchStart, entries)
	fmt.Printf("log  → %s\n", logPath)
}

// ---- Batch timing log ---------------------------------------------------

type batchEntry struct {
	Index    int
	Label    string    // filename (STT) or text snippet (TTS)
	Start    time.Time
	End      time.Time
	OK       bool
	Note     string // output path on success, error message on failure
	Retries  int    // retries used beyond first attempt
	ErrCodes []string // server error codes seen during session
}

func writeBatchLog(path string, batchStart time.Time, entries []batchEntry) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Printf("  LOG ERROR: %v\n", err)
		return
	}
	defer f.Close()

	total := len(entries)
	ok, fail, retried := 0, 0, 0
	errReasons := map[string]int{}
	serverErrCodes := map[string]int{} // code → count across all entries
	for _, e := range entries {
		if e.OK {
			ok++
		} else {
			fail++
			reason := classifyErr(e.Note)
			errReasons[reason]++
		}
		if e.Retries > 0 {
			retried++
		}
		for _, code := range e.ErrCodes {
			serverErrCodes[code]++
		}
	}
	batchEnd := time.Now()

	fmt.Fprintf(f, "Batch run:  %s\n", batchStart.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(f, "Completed:  %s\n", batchEnd.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(f, "Duration:   %s\n", batchEnd.Sub(batchStart).Round(time.Millisecond))
	fmt.Fprintf(f, "Total:      %d items, %d ok, %d failed\n", total, ok, fail)
	fmt.Fprintf(f, "Retried:    %d files needed at least one retry\n", retried)

	if len(errReasons) > 0 {
		fmt.Fprintf(f, "\nFailure breakdown:\n")
		for reason, count := range errReasons {
			fmt.Fprintf(f, "  %-40s %d\n", reason, count)
		}
	}

	if len(serverErrCodes) > 0 {
		fmt.Fprintf(f, "\nServer error codes (across all attempts):\n")
		for code, count := range serverErrCodes {
			fmt.Fprintf(f, "  code=%-30s %d occurrences\n", code, count)
		}
	}

	fmt.Fprintf(f, "\n")

	for _, e := range entries {
		dur := e.End.Sub(e.Start).Round(time.Millisecond)
		status := "OK"
		if !e.OK {
			status = "FAIL"
		}
		fmt.Fprintf(f, "[%02d] %s\n", e.Index, e.Label)
		fmt.Fprintf(f, "     start:    %s\n", e.Start.Format("15:04:05.000"))
		fmt.Fprintf(f, "     end:      %s\n", e.End.Format("15:04:05.000"))
		fmt.Fprintf(f, "     duration: %s\n", dur)
		fmt.Fprintf(f, "     retries:  %d\n", e.Retries)
		if len(e.ErrCodes) > 0 {
			fmt.Fprintf(f, "     errcodes: %v\n", e.ErrCodes)
		}
		fmt.Fprintf(f, "     status:   %s  %s\n\n", status, e.Note)
	}
}

func classifyErr(note string) string {
	switch {
	case strings.Contains(note, "no transcript"):
		return "no transcript (empty/timeout)"
	case strings.Contains(note, "ws connect"):
		return "gateway unreachable"
	case strings.Contains(note, "send audio"):
		return "connection dropped mid-stream"
	case strings.Contains(note, "read welcome"):
		return "gateway handshake failed"
	default:
		return "other: " + note
	}
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
