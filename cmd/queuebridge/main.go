package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/config"
	"github.com/Yooouuuuuuu/flowdispatch/internal/broker"
	"github.com/Yooouuuuuuu/flowdispatch/internal/gateway"
	"github.com/Yooouuuuuuu/flowdispatch/internal/stt"
	"github.com/Yooouuuuuuu/flowdispatch/internal/tts"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: queuebridge <serve|test-stt|test-tts|test-both>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfgFile := fs.String("config", "", "path to YAML config file")
		addr := fs.String("addr", "", "listen address override (e.g. :9090)")
		fs.Parse(os.Args[2:])

		var cfg *config.Config
		if *cfgFile != "" {
			var err error
			cfg, err = config.LoadFile(*cfgFile)
			if err != nil {
				log.Fatalf("config: %v", err)
			}
			log.Printf("[main] config loaded from %s", *cfgFile)
		} else {
			cfg = config.Load()
		}

		if *addr != "" {
			cfg.Listen = *addr
		}

		serveGateway(cfg)

	case "test-stt":
		testSTT(config.Load())
	case "test-tts":
		testTTS(config.Load())
	case "test-both":
		cfg := config.Load()
		testSTT(cfg)
		fmt.Println()
		testTTS(cfg)
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

func serveGateway(cfg *config.Config) {
	if len(cfg.Pools) == 0 {
		log.Println("[main] no pools configured — add a pools section to your config file")
	}
	for _, p := range cfg.Pools {
		log.Printf("[main] pool: %-12s  service=%-4s  protocol=%-5s  conns=%d",
			p.Name, p.Service, p.Protocol, p.Conns)
	}

	// sigCtx is cancelled on SIGTERM/SIGINT — stops the gateway from accepting
	// new connections and new job submissions.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// brokerCtx is not tied to the signal so in-flight jobs can finish during drain.
	brokerCtx, brokerCancel := context.WithCancel(context.Background())
	defer brokerCancel()

	b := broker.New(*cfg, cfg.Pools)
	if err := b.Start(brokerCtx); err != nil {
		log.Fatalf("broker start: %v", err)
	}

	gw := gateway.New(cfg.Listen, b)
	if err := gw.Start(sigCtx); err != nil {
		log.Fatalf("gateway error: %v", err)
	}

	log.Println("[main] gateway stopped — draining in-flight jobs (30s timeout)…")
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer drainCancel()
	if err := b.Drain(drainCtx); err != nil {
		log.Printf("[main] drain timed out (%v) — forcing shutdown", err)
	} else {
		log.Println("[main] all jobs drained")
	}
	brokerCancel()
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

	fmt.Println("Starting recognition...")
	if err := client.StartRecognition(); err != nil {
		log.Printf("Failed to start recognition: %v", err)
		return
	}
	fmt.Println("Recognition started - ready to receive audio")

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

	testText := "Hello, this is a test."
	fmt.Printf("Synthesizing: %s\n", testText)

	audioData, err := client.Synthesize(ctx, testText)
	if err != nil {
		log.Printf("TTS synthesis FAILED: %v", err)
		return
	}

	fmt.Printf("TTS synthesis SUCCESS - received %d bytes of audio\n", len(audioData))

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
