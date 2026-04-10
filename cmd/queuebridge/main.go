package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/config"
	"github.com/Yooouuuuuuu/flowdispatch/internal/broker"
	"github.com/Yooouuuuuuu/flowdispatch/internal/gateway"
	"github.com/Yooouuuuuuu/flowdispatch/internal/stt"
	"github.com/Yooouuuuuuu/flowdispatch/internal/tts"
)

func main() {
	cfg := config.Load()

	if len(os.Args) < 2 {
		fmt.Println("Usage: queuebridge <serve|test-stt|test-tts|test-both>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		addr := fs.String("addr", ":8080", "listen address")

		// --pool name:service:protocol:conns  (repeatable)
		var pools poolFlags
		fs.Var(&pools, "pool", "pool definition: name:service:protocol:conns (repeatable)\n"+
			"  e.g. --pool stt-a:stt:ws:2 --pool tts-a:tts:grpc:1")

		// legacy shortcuts for convenience
		sttConns := fs.Int("stt", 0, "shorthand: add a pool named 'stt-default' with N WebSocket workers")
		ttsConns := fs.Int("tts", 0, "shorthand: add a pool named 'tts-default' with N gRPC workers")

		fs.Parse(os.Args[2:])

		if *sttConns > 0 {
			pools = append(pools, broker.PoolConfig{Name: "stt-default", Service: "stt", Protocol: "ws", Conns: *sttConns})
		}
		if *ttsConns > 0 {
			pools = append(pools, broker.PoolConfig{Name: "tts-default", Service: "tts", Protocol: "grpc", Conns: *ttsConns})
		}

		serveGateway(*addr, []broker.PoolConfig(pools))

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

// poolFlags is a repeatable --pool flag.
type poolFlags []broker.PoolConfig

func (f *poolFlags) String() string {
	parts := make([]string, len(*f))
	for i, p := range *f {
		parts[i] = fmt.Sprintf("%s:%s:%s:%d", p.Name, p.Service, p.Protocol, p.Conns)
	}
	return strings.Join(parts, ", ")
}

func (f *poolFlags) Set(v string) error {
	parts := strings.SplitN(v, ":", 4)
	if len(parts) != 4 {
		return fmt.Errorf("pool flag must be name:service:protocol:conns, got %q", v)
	}
	conns, err := strconv.Atoi(parts[3])
	if err != nil || conns <= 0 {
		return fmt.Errorf("conns must be a positive integer, got %q", parts[3])
	}
	*f = append(*f, broker.PoolConfig{
		Name:     parts[0],
		Service:  parts[1],
		Protocol: parts[2],
		Conns:    conns,
	})
	return nil
}

func serveGateway(addr string, poolCfgs []broker.PoolConfig) {
	if len(poolCfgs) == 0 {
		log.Println("[main] no pools configured — use --pool or --stt/--tts flags")
	}
	for _, p := range poolCfgs {
		log.Printf("[main] pool: %-12s  service=%-4s  protocol=%-5s  conns=%d",
			p.Name, p.Service, p.Protocol, p.Conns)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()

	b := broker.New(*cfg, poolCfgs)
	if err := b.Start(ctx); err != nil {
		log.Fatalf("broker start: %v", err)
	}

	gw := gateway.New(addr, b)
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
