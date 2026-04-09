package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Yooouuuuuuu/queuebridge/config"
	"github.com/Yooouuuuuuu/queuebridge/internal/gateway"
	"github.com/Yooouuuuuuu/queuebridge/internal/stt"
	"github.com/Yooouuuuuuu/queuebridge/internal/tts"
)

func main() {
	cfg := config.Load()

	if len(os.Args) < 2 {
		fmt.Println("Usage: queuebridge <serve|test-stt|test-tts|test-both>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		addr := ":8080"
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		serveGateway(addr)
	case "test-stt":
		testSTT(cfg)
	case "test-tts":
		testTTS(cfg)
	case "test-both":
		testSTT(cfg)
		fmt.Println()
		testTTS(cfg)
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func serveGateway(addr string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()
	gw := gateway.New(addr, *cfg)
	if err := gw.Start(ctx); err != nil {
		log.Fatalf("gateway error: %v", err)
	}
	log.Println("gateway stopped")
}

func testSTT(cfg *config.Config) {
	fmt.Println("=== Testing STT Connection ===")

	client := stt.NewClient(stt.Config{
		Endpoint: cfg.STT.Endpoint,
		Token:    cfg.STT.Token,
		UID:      cfg.STT.UID,
		Domain:   cfg.STT.Domain,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Printf("Connecting to STT: %s\n", cfg.STT.Endpoint)
	if err := client.Connect(ctx); err != nil {
		log.Printf("STT connection FAILED: %v", err)
		return
	}
	defer client.Close()

	fmt.Println("STT connection SUCCESS - received handshake")
	fmt.Printf("  Connected: %v\n", client.IsConnected())
	fmt.Printf("  Listening: %v\n", client.IsListening())

	// Set up result handlers
	client.OnResult(func(text string, isFinal bool) {
		finalStr := "partial"
		if isFinal {
			finalStr = "FINAL"
		}
		fmt.Printf("  [%s] %s\n", finalStr, text)
	})

	client.OnError(func(code int, msg string) {
		fmt.Printf("  [ERROR] code=%d msg=%s\n", code, msg)
	})

	// Start recognition
	fmt.Println("Starting recognition...")
	if err := client.StartRecognition(); err != nil {
		log.Printf("Failed to start recognition: %v", err)
		return
	}
	fmt.Println("Recognition started - ready to receive audio")

	// In a real scenario, you would send audio data here:
	// client.SendAudio(pcmData) or client.SendAudioChunk(chunk)

	// Stop recognition
	fmt.Println("Stopping recognition...")
	if err := client.StopRecognition(); err != nil {
		log.Printf("Failed to stop recognition: %v", err)
		return
	}
	fmt.Println("Recognition stopped")

	fmt.Println("STT test completed successfully!")
}

func testTTS(cfg *config.Config) {
	fmt.Println("=== Testing TTS Connection ===")

	client := tts.NewClient(tts.Config{
		Endpoint:    cfg.TTS.Endpoint,
		Token:       cfg.TTS.Token,
		UID:         cfg.TTS.UID,
		ServiceName: cfg.TTS.ServiceName,
		Speaker:     cfg.TTS.Speaker,
		Language:    cfg.TTS.Language,
		OutFormat:   cfg.TTS.OutFormat,
		VBRQuality:  cfg.TTS.VBRQuality,
		Speed:       cfg.TTS.Speed,
		Gain:        cfg.TTS.Gain,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("Connecting to TTS: %s (gRPC+TLS)\n", cfg.TTS.Endpoint)
	if err := client.Connect(ctx); err != nil {
		log.Printf("TTS connection FAILED: %v", err)
		return
	}
	defer client.Close()

	fmt.Println("TTS connection SUCCESS")
	fmt.Printf("  Connected: %v\n", client.IsConnected())

	// Test synthesis
	testText := "Hello, this is a test."
	fmt.Printf("Synthesizing: %s\n", testText)

	audioData, err := client.Synthesize(ctx, testText)
	if err != nil {
		log.Printf("TTS synthesis FAILED: %v", err)
		return
	}

	fmt.Printf("TTS synthesis SUCCESS - received %d bytes of audio\n", len(audioData))

	// Optionally save to file for verification
	if len(os.Args) > 2 && os.Args[2] == "--save" {
		outFile := "test_output.wav"
		if err := os.WriteFile(outFile, audioData, 0644); err != nil {
			log.Printf("Failed to save audio: %v", err)
		} else {
			fmt.Printf("Audio saved to: %s\n", outFile)
		}
	}

	fmt.Println("TTS test completed successfully!")
}
