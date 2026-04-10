// Package broker maintains persistent backend connection pools and routes
// jobs through a priority queue to the appropriate pool.
package broker

import (
	"container/heap"
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/config"
	"github.com/Yooouuuuuuu/flowdispatch/internal/stt"
	"github.com/Yooouuuuuuu/flowdispatch/internal/tts"
)

// ---- Job types ----

// Job is the unified job descriptor for all services.
type Job struct {
	Service  string // "stt" | "tts"
	Pool     string // target pool name; "" = auto-route (least-loaded)
	Priority int    // 0 = normal, 9 = highest urgency
	Payload  any    // *STTPayload | *TTSPayload
	ResultCh chan Result
}

// STTPayload carries an audio stream for an STT job.
type STTPayload struct {
	AudioCh <-chan []byte // closed by caller when no more audio
}

// TTSPayload carries text for a TTS job.
type TTSPayload struct {
	Text string
}

// Result is one message delivered back to the job submitter.
// ResultCh is closed by the broker when the job is fully complete.
type Result struct {
	Text    string // STT: transcript chunk
	IsFinal bool   // STT: true = final result
	Audio   []byte // TTS: synthesised audio bytes
	ErrCode int    // non-zero = error
	ErrMsg  string
}

// ---- Pool config ----

// PoolConfig describes one named pool of backend workers.
type PoolConfig struct {
	Name     string // unique identifier, e.g. "stt-a"
	Service  string // "stt" | "tts"
	Protocol string // "ws" | "grpc"
	Conns    int    // number of persistent worker connections
}

// ---- Priority queue ----

type jobItem struct {
	job Job
	pri int   // higher = dispatched first
	seq int64 // FIFO tiebreaker; lower = enqueued earlier
}

type jobHeap []jobItem

func (h jobHeap) Len() int { return len(h) }
func (h jobHeap) Less(i, j int) bool {
	if h[i].pri != h[j].pri {
		return h[i].pri > h[j].pri // max-heap: higher priority first
	}
	return h[i].seq < h[j].seq // FIFO within same priority
}
func (h jobHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *jobHeap) Push(x any)         { *h = append(*h, x.(jobItem)) }
func (h *jobHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type priorityQueue struct {
	mu  sync.Mutex
	cond *sync.Cond
	h    jobHeap
	seq  atomic.Int64
}

func newPriorityQueue() *priorityQueue {
	pq := &priorityQueue{}
	pq.cond = sync.NewCond(&pq.mu)
	heap.Init(&pq.h)
	return pq
}

// push enqueues a job. Always succeeds; never blocks.
func (pq *priorityQueue) push(job Job) {
	pq.mu.Lock()
	heap.Push(&pq.h, jobItem{job: job, pri: job.Priority, seq: pq.seq.Add(1)})
	pq.cond.Signal()
	pq.mu.Unlock()
}

// pop blocks until a job is available or ctx is cancelled.
// Returns (job, true) on success, (zero, false) if ctx was cancelled.
func (pq *priorityQueue) pop(ctx context.Context) (Job, bool) {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	for pq.h.Len() == 0 {
		pq.cond.Wait()
		if ctx.Err() != nil {
			return Job{}, false
		}
	}
	return heap.Pop(&pq.h).(jobItem).job, true
}

// len returns the current queue depth.
func (pq *priorityQueue) len() int {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	return pq.h.Len()
}

// ---- Pool ----

type pool struct {
	cfg    PoolConfig
	queue  *priorityQueue
	active atomic.Int32 // jobs currently running
}

// ---- Broker ----

// Broker holds named pools and routes incoming jobs through priority queues.
type Broker struct {
	cfg      config.Config
	pools    map[string]*pool   // name → pool
	services map[string][]*pool // service → ordered list of pools
}

// New creates a Broker from a list of pool configs. Call Start to connect.
func New(cfg config.Config, poolCfgs []PoolConfig) *Broker {
	b := &Broker{
		cfg:      cfg,
		pools:    make(map[string]*pool),
		services: make(map[string][]*pool),
	}
	for _, pc := range poolCfgs {
		p := &pool{cfg: pc, queue: newPriorityQueue()}
		b.pools[pc.Name] = p
		b.services[pc.Service] = append(b.services[pc.Service], p)
	}
	return b
}

// Start connects all pools and launches background workers.
func (b *Broker) Start(ctx context.Context) error {
	for _, p := range b.pools {
		switch p.cfg.Service {
		case "tts":
			if err := b.startTTSPool(ctx, p); err != nil {
				return err
			}
		case "stt":
			if err := b.startSTTPool(ctx, p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown service %q in pool %q", p.cfg.Service, p.cfg.Name)
		}
		// wake blocked workers when ctx ends
		go func(pq *priorityQueue) {
			<-ctx.Done()
			pq.cond.Broadcast()
		}(p.queue)
	}
	go b.runStatusLogger(ctx)
	return nil
}

// ---- TTS pool ----

func (b *Broker) startTTSPool(ctx context.Context, p *pool) error {
	for i := range p.cfg.Conns {
		cli := tts.NewClient(tts.Config{
			Endpoint:    b.cfg.TTS.Endpoint,
			Token:       b.cfg.TTS.Token,
			UID:         b.cfg.TTS.UID,
			ServiceName: b.cfg.TTS.ServiceName,
			Speaker:     b.cfg.TTS.Speaker,
			Language:    b.cfg.TTS.Language,
			OutFormat:   b.cfg.TTS.OutFormat,
			VBRQuality:  b.cfg.TTS.VBRQuality,
			Speed:       b.cfg.TTS.Speed,
			Gain:        b.cfg.TTS.Gain,
		})
		connCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := cli.Connect(connCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("pool %q TTS[%d] connect: %w", p.cfg.Name, i, err)
		}
		log.Printf("[broker] pool=%s  TTS[%d] connected", p.cfg.Name, i)
		go b.runTTSWorker(ctx, p, cli)
	}
	return nil
}

func (b *Broker) runTTSWorker(ctx context.Context, p *pool, cli *tts.Client) {
	defer cli.Close()
	for {
		job, ok := p.queue.pop(ctx)
		if !ok {
			return // ctx cancelled
		}
		payload, ok := job.Payload.(*TTSPayload)
		if !ok {
			log.Printf("[broker] pool=%s  unexpected TTS payload type", p.cfg.Name)
			close(job.ResultCh)
			continue
		}

		p.active.Add(1)
		audio, err := cli.Synthesize(ctx, payload.Text)
		p.active.Add(-1)

		if err != nil {
			job.ResultCh <- Result{ErrCode: 1, ErrMsg: err.Error()}
		} else {
			job.ResultCh <- Result{Audio: audio}
		}
		close(job.ResultCh)
	}
}

// ---- STT pool ----

func (b *Broker) startSTTPool(ctx context.Context, p *pool) error {
	for i := range p.cfg.Conns {
		cli, err := b.dialSTT(ctx, p.cfg.Name)
		if err != nil {
			return err
		}
		log.Printf("[broker] pool=%s  STT[%d] connected", p.cfg.Name, i)
		go b.runSTTWorker(ctx, p, cli)
	}
	return nil
}

func (b *Broker) dialSTT(ctx context.Context, poolName string) (*stt.Client, error) {
	for {
		cli := stt.NewClient(stt.Config{
			Endpoint: b.cfg.STT.Endpoint,
			Token:    b.cfg.STT.Token,
			UID:      b.cfg.STT.UID,
			Domain:   b.cfg.STT.Domain,
		})
		if err := cli.Connect(ctx); err == nil {
			return cli, nil
		} else {
			log.Printf("[broker] pool=%s  STT connect failed: %v, retrying in 5s", poolName, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *Broker) runSTTWorker(ctx context.Context, p *pool, cli *stt.Client) {
	defer cli.Close()
	for {
		job, ok := p.queue.pop(ctx)
		if !ok {
			return // ctx cancelled
		}
		dropped := b.processSTTSession(ctx, p, cli, job)
		if dropped {
			cli.Close()
			var err error
			cli, err = b.dialSTT(ctx, p.cfg.Name)
			if err != nil {
				return // ctx cancelled
			}
			log.Printf("[broker] pool=%s  STT reconnected", p.cfg.Name)
		}
	}
}

func (b *Broker) processSTTSession(ctx context.Context, p *pool, cli *stt.Client, job Job) (connDropped bool) {
	payload, ok := job.Payload.(*STTPayload)
	if !ok {
		log.Printf("[broker] pool=%s  unexpected STT payload type", p.cfg.Name)
		close(job.ResultCh)
		return false
	}

	p.active.Add(1)
	defer p.active.Add(-1)

	sessionDone := make(chan struct{})
	var once sync.Once
	signalDone := func() { once.Do(func() { close(sessionDone) }) }

	cli.OnResult(func(text string, isFinal bool) {
		select {
		case job.ResultCh <- Result{Text: text, IsFinal: isFinal}:
		case <-sessionDone:
		}
		if isFinal {
			signalDone()
		}
	})
	cli.OnError(func(code int, msg string) {
		select {
		case job.ResultCh <- Result{ErrCode: code, ErrMsg: msg}:
		case <-sessionDone:
		}
	})

	readCtx, cancelRead := context.WithCancel(ctx)
	connErrCh := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		if err := cli.ReadMessages(readCtx); readCtx.Err() == nil && err != nil {
			connErrCh <- err
			signalDone()
		}
	}()

	if err := cli.StartRecognition(); err != nil {
		log.Printf("[broker] pool=%s  StartRecognition: %v", p.cfg.Name, err)
		cancelRead()
		<-readDone
		close(job.ResultCh)
		return true
	}

	go func() {
		for chunk := range payload.AudioCh {
			if err := cli.SendAudioChunk(chunk); err != nil {
				log.Printf("[broker] pool=%s  SendAudioChunk: %v", p.cfg.Name, err)
				break
			}
		}
		if err := cli.StopRecognition(); err != nil {
			log.Printf("[broker] pool=%s  StopRecognition: %v", p.cfg.Name, err)
		}
	}()

	select {
	case <-sessionDone:
	case <-time.After(30 * time.Second):
		log.Printf("[broker] pool=%s  STT session timed out", p.cfg.Name)
	case <-ctx.Done():
		cancelRead()
		<-readDone
		close(job.ResultCh)
		return false
	}

	// Cancel the read loop and wait for it to stop before closing ResultCh.
	// This prevents the OnResult/OnError callbacks (called from ReadMessages)
	// from racing with close(ResultCh) and causing a send-on-closed panic.
	cancelRead()
	<-readDone

	select {
	case err := <-connErrCh:
		log.Printf("[broker] pool=%s  STT connection dropped: %v", p.cfg.Name, err)
		close(job.ResultCh)
		return true
	default:
		close(job.ResultCh)
		return false
	}
}

// ---- Routing ----

// Submit enqueues a job. Always succeeds (blocking submit, no rejection).
// Returns an error only if the target pool doesn't exist or no pool serves
// the requested service.
func (b *Broker) Submit(job Job) error {
	p, err := b.resolvePool(job)
	if err != nil {
		return err
	}
	p.queue.push(job)
	return nil
}

func (b *Broker) resolvePool(job Job) (*pool, error) {
	if job.Pool != "" {
		p, ok := b.pools[job.Pool]
		if !ok {
			return nil, fmt.Errorf("unknown pool %q", job.Pool)
		}
		return p, nil
	}
	return b.leastLoaded(job.Service)
}

func (b *Broker) leastLoaded(service string) (*pool, error) {
	pools := b.services[service]
	if len(pools) == 0 {
		return nil, fmt.Errorf("no pool configured for service %q", service)
	}
	best := pools[0]
	for _, p := range pools[1:] {
		if p.active.Load() < best.active.Load() {
			best = p
		}
	}
	return best, nil
}

// ---- Status logger ----

func (b *Broker) runStatusLogger(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for name, p := range b.pools {
				log.Printf("[broker] pool=%-12s  workers=%d  active=%d  queued=%d",
					name, p.cfg.Conns, p.active.Load(), p.queue.len())
			}
		}
	}
}
