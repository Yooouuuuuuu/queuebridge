package grpcgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/Yooouuuuuuu/flowdispatch/internal/broker"
	pb "github.com/Yooouuuuuuu/flowdispatch/proto"
)

// Server is the gRPC inbound gateway. It implements pb.FlowDispatchServer
// and mirrors the behaviour of the HTTP and WebSocket gateways.
type Server struct {
	pb.UnimplementedFlowDispatchServer
	broker   *broker.Broker
	sessions sessionRegistry
	grpc     *grpc.Server
}

// New creates a gRPC gateway server.
func New(b *broker.Broker) *Server {
	return &Server{
		broker:   b,
		sessions: sessionRegistry{sessions: make(map[string]*session)},
	}
}

// Start begins listening on addr (e.g. ":9090"). Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc listen %s: %w", addr, err)
	}
	s.grpc = grpc.NewServer()
	pb.RegisterFlowDispatchServer(s.grpc, s)
	reflection.Register(s.grpc)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[grpc] listening on %s", addr)
		if err := s.grpc.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.grpc.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

// ---- Submit (unary) ----

func (s *Server) Submit(ctx context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	switch req.Service {
	case "tts":
		return s.submitTTS(ctx, req)
	case "stt":
		return s.submitSTT(ctx, req)
	default:
		return nil, status.Errorf(codes.Unimplemented,
			"service %q not configured", req.Service)
	}
}

func (s *Server) submitTTS(ctx context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	if req.Text == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}

	resultCh := make(chan broker.Result, 1)
	sr, err := s.broker.Submit(broker.Job{
		Service:  "tts",
		Pool:     req.Pool,
		Priority: int(req.Priority),
		Payload: &broker.TTSPayload{
			Text:        req.Text,
			Speaker:     req.Speaker,
			Language:    req.Language,
			OutFormat:   req.OutFormat,
			Speed:       req.Speed,
			Gain:        req.Gain,
			PhraseBreak: req.PhraseBreak,
		},
		ResultCh: resultCh,
	})
	if err != nil {
		return nil, classifyError(err)
	}

	log.Printf("[grpc/tts] pool=%s%s: %q", sr.Pool, warningLabel(sr.Warning), req.Text)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	select {
	case res, ok := <-resultCh:
		if !ok || res.ErrCode != 0 {
			msg := res.ErrMsg
			if !ok {
				msg = "no result received"
			}
			return nil, status.Error(codes.Internal, msg)
		}
		return &pb.SubmitResponse{
			PoolUsed: sr.Pool,
			Warning:  sr.Warning,
			Audio:    res.Audio,
		}, nil
	case <-ctx.Done():
		return nil, status.Error(codes.DeadlineExceeded, "TTS synthesis timed out")
	}
}

func (s *Server) submitSTT(ctx context.Context, req *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	if len(req.Audio) == 0 {
		return nil, status.Error(codes.InvalidArgument, "audio is required")
	}

	audioCh := make(chan []byte, 64)
	resultCh := make(chan broker.Result, 32)
	readyCh := make(chan struct{})

	sr, err := s.broker.Submit(broker.Job{
		Service:  "stt",
		Pool:     req.Pool,
		Priority: int(req.Priority),
		Payload:  &broker.STTPayload{AudioCh: audioCh},
		ResultCh: resultCh,
		ReadyCh:  readyCh,
	})
	if err != nil {
		return nil, classifyError(err)
	}

	log.Printf("[grpc/stt] pool=%s%s (%d bytes)", sr.Pool, warningLabel(sr.Warning), len(req.Audio))

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	go func() {
		select {
		case <-readyCh:
		case <-ctx.Done():
			close(audioCh)
			return
		}
		const chunkSize = 4096
		buf := req.Audio
		for i := 0; i < len(buf); i += chunkSize {
			end := i + chunkSize
			if end > len(buf) {
				end = len(buf)
			}
			select {
			case audioCh <- buf[i:end]:
			case <-ctx.Done():
				close(audioCh)
				return
			}
		}
		close(audioCh)
	}()

	var transcript string
	for {
		select {
		case res, ok := <-resultCh:
			if !ok {
				return &pb.SubmitResponse{
					PoolUsed:   sr.Pool,
					Warning:    sr.Warning,
					Transcript: transcript,
				}, nil
			}
			if res.ErrCode != 0 {
				return nil, status.Error(codes.Internal, res.ErrMsg)
			}
			if res.IsFinal {
				transcript += res.Text
			}
		case <-ctx.Done():
			return nil, status.Error(codes.DeadlineExceeded, "STT recognition timed out")
		}
	}
}

// ---- Stream (bidirectional streaming) ----

func (s *Server) Stream(stream pb.FlowDispatch_StreamServer) error {
	ctx := stream.Context()
	log.Printf("[grpc/stream] client connected")

	var (
		jobActive  atomic.Bool
		curAudioCh chan []byte
		closeAudio func()
		sess       *session
	)

	defer func() {
		if closeAudio != nil {
			closeAudio()
		}
		if sess != nil {
			s.sessions.remove(sess.id)
		}
	}()

	if err := stream.Send(&pb.StreamMessage{Type: "connected"}); err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Printf("[grpc/stream] recv error: %v", err)
			return err
		}

		switch msg.Type {
		case "start":
			if jobActive.Load() {
				continue
			}

			if msg.SessionType != "" && sess == nil {
				sess = s.sessions.create(msg.SessionType)
				// gRPC has built-in keepalive; no manual ping needed
			}

			pool := msg.Pool
			if pool == "" && sess != nil && sess.stickyPool != "" {
				pool = sess.stickyPool
			}

			isSessionOriented := sess != nil
			resultCh := make(chan broker.Result, 32)
			var readyCh chan struct{}

			switch msg.Service {
			case "stt":
				audioCh := make(chan []byte, 256)
				once := &sync.Once{}
				curAudioCh = audioCh
				closeAudio = func() { once.Do(func() { close(audioCh) }) }
				readyCh = make(chan struct{})

				sr, err := s.broker.Submit(broker.Job{
					Service:  "stt",
					Pool:     pool,
					Priority: int(msg.Priority),
					Payload:  &broker.STTPayload{AudioCh: audioCh},
					ResultCh: resultCh,
					ReadyCh:  readyCh,
				})
				if err != nil {
					stream.Send(&pb.StreamMessage{Type: "error", Code: grpcErrCode(err), Msg: err.Error()})
					if !isSessionOriented {
						return nil
					}
					continue
				}

				if sess != nil && sess.stickyPool == "" {
					sess.stickyPool = sr.Pool
				}
				log.Printf("[grpc/stream] stt queued pool=%s%s", sr.Pool, warningLabel(sr.Warning))
				s.streamPostSubmit(ctx, stream, sr, resultCh, readyCh, isSessionOriented, &jobActive, sess)

			case "tts":
				if msg.Text == "" {
					stream.Send(&pb.StreamMessage{Type: "error", Code: "bad_request", Msg: "text is required for tts"})
					continue
				}
				curAudioCh = nil
				closeAudio = func() {}

				sr, err := s.broker.Submit(broker.Job{
					Service:  "tts",
					Pool:     pool,
					Priority: int(msg.Priority),
					Payload: &broker.TTSPayload{
						Text:        msg.Text,
						Speaker:     msg.Speaker,
						Language:    msg.Language,
						OutFormat:   msg.OutFormat,
						Speed:       msg.Speed,
						Gain:        msg.Gain,
						PhraseBreak: msg.PhraseBreak,
					},
					ResultCh: resultCh,
				})
				if err != nil {
					stream.Send(&pb.StreamMessage{Type: "error", Code: grpcErrCode(err), Msg: err.Error()})
					if !isSessionOriented {
						return nil
					}
					continue
				}

				if sess != nil && sess.stickyPool == "" {
					sess.stickyPool = sr.Pool
				}
				log.Printf("[grpc/stream] tts queued pool=%s%s", sr.Pool, warningLabel(sr.Warning))
				s.streamPostSubmit(ctx, stream, sr, resultCh, nil, isSessionOriented, &jobActive, sess)

			default:
				_, err := s.broker.Submit(broker.Job{Service: msg.Service, ResultCh: resultCh})
				if err != nil {
					stream.Send(&pb.StreamMessage{Type: "error", Code: grpcErrCode(err), Msg: err.Error()})
				}
				close(resultCh)
				if !isSessionOriented {
					return nil
				}
				continue
			}

			jobActive.Store(true)

		case "stop":
			if jobActive.Load() && closeAudio != nil {
				closeAudio()
			}

		case "audio":
			if jobActive.Load() && curAudioCh != nil {
				select {
				case curAudioCh <- msg.Audio:
				default:
					log.Printf("[grpc/stream] audio buffer full, dropping chunk")
				}
			}
		}
	}
}

// streamPostSubmit sends warning/ready frames and starts the result goroutine.
func (s *Server) streamPostSubmit(
	ctx context.Context,
	stream pb.FlowDispatch_StreamServer,
	sr broker.SubmitResult,
	resultCh <-chan broker.Result,
	readyCh <-chan struct{},
	isSessionOriented bool,
	jobActive *atomic.Bool,
	sess *session,
) {
	if sr.Warning != "" {
		stream.Send(&pb.StreamMessage{Type: "warning", Msg: sr.Warning})
	}

	if readyCh != nil {
		go func() {
			select {
			case <-readyCh:
				stream.Send(&pb.StreamMessage{Type: "ready"})
			case <-ctx.Done():
			}
		}()
	}

	go func() {
		for res := range resultCh {
			if res.ErrCode != 0 {
				stream.Send(&pb.StreamMessage{Type: "error", Code: "upstream_failed", Msg: res.ErrMsg})
			} else if res.Audio != nil {
				stream.Send(&pb.StreamMessage{Type: "result", ResultAudio: res.Audio})
			} else {
				stream.Send(&pb.StreamMessage{Type: "result", ResultText: res.Text, Final: res.IsFinal})
			}
		}
		jobActive.Store(false)
		stream.Send(&pb.StreamMessage{Type: "done"})
		if sess != nil {
			sess.lastSeen = time.Now()
		}
	}()
}

// ---- Session registry ----

type session struct {
	id         string
	clientType string
	stickyPool string
	lastSeen   time.Time
}

type sessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*session
}

func (r *sessionRegistry) create(clientType string) *session {
	s := &session{
		id:         fmt.Sprintf("%x", time.Now().UnixNano()),
		clientType: clientType,
		lastSeen:   time.Now(),
	}
	r.mu.Lock()
	r.sessions[s.id] = s
	r.mu.Unlock()
	log.Printf("[grpc/session] created id=%s type=%s", s.id, s.clientType)
	return s
}

func (r *sessionRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
	log.Printf("[grpc/session] removed id=%s", id)
}

// ---- Helpers ----

func classifyError(err error) error {
	switch {
	case errors.Is(err, broker.ErrServiceNotConfigured):
		return status.Errorf(codes.Unimplemented, err.Error())
	case errors.Is(err, broker.ErrDraining):
		return status.Errorf(codes.Unavailable, err.Error())
	default:
		return status.Errorf(codes.Internal, err.Error())
	}
}

func grpcErrCode(err error) string {
	switch {
	case errors.Is(err, broker.ErrServiceNotConfigured):
		return "service_unavailable"
	case errors.Is(err, broker.ErrDraining):
		return "shutting_down"
	default:
		return "submit_failed"
	}
}

func warningLabel(w string) string {
	if w == "" {
		return ""
	}
	return " [warning: " + w + "]"
}
