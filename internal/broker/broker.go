// Package broker maintains persistent backend connection pools and routes
// jobs through a priority queue to the appropriate pool.
package broker

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Yooouuuuuuu/flowdispatch/config"
	"github.com/Yooouuuuuuu/flowdispatch/internal/stt"
	"github.com/Yooouuuuuuu/flowdispatch/internal/tts"
)

// PoolConfig is re-exported from the config package for callers that only
// import broker.
type PoolConfig = config.PoolConfig

// Sentinel errors for gateway-level error classification.
var (
	ErrServiceNotConfigured = errors.New("service not configured")
	ErrDraining             = errors.New("server is shutting down")
)

// ---- Job types ----

// Job is the unified job descriptor for all services.
type Job struct {
	Service  string      // "stt" | "tts"
	Pool     string      // target pool name; "" = auto-route (least-loaded)
	Priority int         // 0 = normal, 9 = highest urgency
	Payload  any         // *STTPayload | *TTSPayload
	ResultCh chan Result
	ReadyCh  chan struct{} // closed by broker when a session dequeues this job; nil = unused
}

// STTPayload carries an audio stream for an STT job.
type STTPayload struct {
	AudioCh <-chan []byte // closed by caller when no more audio
}

// TTSPayload carries text for a TTS job.
// Override fields are optional; zero value means "use the pool's config default".
type TTSPayload struct {
	Text        string
	Speaker     string
	Language    string
	OutFormat   string
	Speed       float32
	Gain        float32
	PhraseBreak bool
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

// SubmitResult carries routing metadata returned by Submit.
type SubmitResult struct {
	Pool    string // actual pool name used
	Warning string // non-empty when fallback routing occurred
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
	mu   sync.Mutex
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

// pop blocks until a job is available, ctx is cancelled, or draining is true
// and the queue is empty. Returns (job, true) on success, (zero, false) otherwise.
func (pq *priorityQueue) pop(ctx context.Context, draining *atomic.Bool) (Job, bool) {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	for pq.h.Len() == 0 {
		if ctx.Err() != nil {
			return Job{}, false
		}
		if draining.Load() {
			return Job{}, false // drain mode: empty queue → worker should exit
		}
		pq.cond.Wait()
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
	cfg       PoolConfig
	queue     *priorityQueue
	active    atomic.Int32 // sessions currently processing audio
	idle      atomic.Int32 // sessions connected and waiting for a job
	completed atomic.Int64 // total jobs completed successfully
	errors    atomic.Int64 // total jobs that returned an error
}

// PoolMetrics is a snapshot of a single pool's current counters.
type PoolMetrics struct {
	Name      string
	Conns     int
	Active    int32
	Idle      int32
	Queued    int
	Completed int64
	Errors    int64
}

// ---- Broker ----

// Broker holds named pools and routes incoming jobs through priority queues.
type Broker struct {
	cfg      config.Config
	pools    map[string]*pool   // name → pool
	services map[string][]*pool // service → ordered list of pools

	draining atomic.Bool
	workerWg sync.WaitGroup
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
		// Wake blocked workers when ctx ends (hard stop).
		go func(pq *priorityQueue) {
			<-ctx.Done()
			pq.cond.Broadcast()
		}(p.queue)
	}
	return nil
}

// Drain stops accepting new jobs and waits for all in-flight workers to finish
// their current job before returning. ctx controls the wait deadline; cancel it
// to force workers to exit immediately.
func (b *Broker) Drain(ctx context.Context) error {
	b.draining.Store(true)
	// Wake all idle workers so they see the drain flag and exit.
	for _, p := range b.pools {
		p.queue.cond.Broadcast()
	}

	done := make(chan struct{})
	go func() {
		b.workerWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- TTS pool ----

func (b *Broker) startTTSPool(ctx context.Context, p *pool) error {
	endpoint := b.cfg.TTS.Endpoint
	if p.cfg.Endpoint != "" {
		endpoint = p.cfg.Endpoint
	}
	for i := range p.cfg.Conns {
		cli := tts.NewClient(tts.Config{
			Endpoint:    endpoint,
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
		b.workerWg.Add(1)
		go func() {
			defer b.workerWg.Done()
			b.runTTSWorker(ctx, p, cli)
		}()
	}
	return nil
}

func (b *Broker) runTTSWorker(ctx context.Context, p *pool, cli *tts.Client) {
	defer cli.Close()
	for {
		job, ok := p.queue.pop(ctx, &b.draining)
		if !ok {
			return
		}
		payload, ok := job.Payload.(*TTSPayload)
		if !ok {
			log.Printf("[broker] pool=%s  unexpected TTS payload type", p.cfg.Name)
			close(job.ResultCh)
			continue
		}

		p.active.Add(1)
		opts := tts.SynthesizeOptions{
			Speaker:     payload.Speaker,
			Language:    payload.Language,
			OutFormat:   payload.OutFormat,
			Speed:       payload.Speed,
			Gain:        payload.Gain,
			PhraseBreak: payload.PhraseBreak,
		}
		audio, err := cli.SynthesizeWithOptions(ctx, payload.Text, opts)
		p.active.Add(-1)

		if err != nil {
			p.errors.Add(1)
			job.ResultCh <- Result{ErrCode: 1, ErrMsg: err.Error()}
		} else {
			p.completed.Add(1)
			job.ResultCh <- Result{Audio: audio}
		}
		close(job.ResultCh)
	}
}

// ---- STT pool ----

func (b *Broker) startSTTPool(ctx context.Context, p *pool) error {
	endpoint := b.cfg.STT.Endpoint
	if p.cfg.Endpoint != "" {
		endpoint = p.cfg.Endpoint
	}
	for i := range p.cfg.Conns {
		cli, err := b.dialSTT(ctx, p.cfg.Name, endpoint)
		if err != nil {
			return err
		}
		log.Printf("[broker] pool=%s  STT[%d] connected", p.cfg.Name, i)
		b.workerWg.Add(1)
		go func() {
			defer b.workerWg.Done()
			b.runSTTWorker(ctx, p, cli, endpoint)
		}()
	}
	return nil
}

func (b *Broker) dialSTT(ctx context.Context, poolName, endpoint string) (*stt.Client, error) {
	for {
		cli := stt.NewClient(stt.Config{
			Endpoint: endpoint,
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

// runSTTWorker keeps a persistent STT session alive for this pool slot.
// StartRecognition is called once per job, just before streaming begins.
// On connection drop or StartRecognition failure, it reconnects — unless
// draining, in which case it exits without reconnecting.
func (b *Broker) runSTTWorker(ctx context.Context, p *pool, cli *stt.Client, endpoint string) {
	for {
		dropped := b.runArmedSession(ctx, p, cli)
		cli.Close()
		if !dropped || ctx.Err() != nil || b.draining.Load() {
			return
		}
		log.Printf("[broker] pool=%s  STT reconnecting…", p.cfg.Name)
		var err error
		cli, err = b.dialSTT(ctx, p.cfg.Name, endpoint)
		if err != nil {
			return // ctx cancelled
		}
		log.Printf("[broker] pool=%s  STT reconnected", p.cfg.Name)
	}
}

// runArmedSession manages the per-connection job loop. It signals idle when
// waiting for a job, calls StartRecognition per job, and returns true if the
// connection dropped and the caller should reconnect.
func (b *Broker) runArmedSession(ctx context.Context, p *pool, cli *stt.Client) (connDropped bool) {
	// cbMu + cbWg protect the per-job callbacks that ReadMessages fires.
	// We swap them between jobs while ReadMessages keeps running on the same connection.
	// cbWg ensures any in-flight callback finishes before we close job.ResultCh.
	var (
		cbMu     sync.Mutex
		cbWg     sync.WaitGroup
		cbResult func(string, bool)
		cbError  func(int, string)
	)

	cli.OnResult(func(text string, isFinal bool) {
		cbMu.Lock()
		h := cbResult
		if h != nil {
			cbWg.Add(1)
		}
		cbMu.Unlock()
		if h != nil {
			defer cbWg.Done()
			h(text, isFinal)
		}
	})
	cli.OnError(func(code int, msg string) {
		cbMu.Lock()
		h := cbError
		if h != nil {
			cbWg.Add(1)
		}
		cbMu.Unlock()
		if h != nil {
			defer cbWg.Done()
			h(code, msg)
		}
	})

	// Single persistent read loop for this connection's lifetime.
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	connErrCh := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		if err := cli.ReadMessages(readCtx); readCtx.Err() == nil && err != nil {
			connErrCh <- err
		}
	}()

	p.idle.Add(1)

	// listeningCh tracks whether the STT server has returned to listening state.
	// It is captured right after each StartRecognition call (which resets the
	// channel internally), giving the server the entire job-processing window to
	// send {"state":"listening"} before we need it for the next job.
	// The initial value is pre-closed by Connect() (server was already listening).
	listeningCh := cli.ListeningCh()

	clearCallbacks := func(signalJobDone func()) {
		// Close jobDone first so any blocked callback send unblocks via that path,
		// then null out the callbacks and wait for any in-flight one to finish.
		signalJobDone()
		cbMu.Lock()
		cbResult = nil
		cbError = nil
		cbMu.Unlock()
		cbWg.Wait()
	}

	for {
		job, ok := p.queue.pop(ctx, &b.draining)
		if !ok {
			p.idle.Add(-1)
			cancelRead()
			<-readDone
			return false // ctx cancelled or draining with empty queue — clean exit
		}

		payload, ok := job.Payload.(*STTPayload)
		if !ok {
			log.Printf("[broker] pool=%s  unexpected STT payload type", p.cfg.Name)
			close(job.ResultCh)
			continue
		}

		p.idle.Add(-1)
		p.active.Add(1)

		// Wait for the server to confirm it is back in listening state.
		// listeningCh was captured right after the previous StartRecognition, so
		// the server has had the entire previous job's processing time to send
		// {"state":"listening"}. In practice this select is instant on a busy queue.
		select {
		case <-listeningCh:
			// server is ready
		case err := <-connErrCh:
			log.Printf("[broker] pool=%s  connection dropped waiting for listening state: %v", p.cfg.Name, err)
			job.ResultCh <- Result{ErrCode: 1, ErrMsg: "connection lost before session start"}
			close(job.ResultCh)
			p.active.Add(-1)
			cancelRead()
			<-readDone
			return true
		case <-ctx.Done():
			close(job.ResultCh)
			p.active.Add(-1)
			cancelRead()
			<-readDone
			return false
		}

		// Server is ready: tell the client it can start streaming.
		if job.ReadyCh != nil {
			close(job.ReadyCh)
		}

		if err := cli.StartRecognition(); err != nil {
			log.Printf("[broker] pool=%s  StartRecognition: %v", p.cfg.Name, err)
			// Notify the client immediately so it fails fast and retries,
			// rather than streaming audio into a void and waiting for the deadline.
			job.ResultCh <- Result{ErrCode: 1, ErrMsg: "recognition session failed: " + err.Error()}
			close(job.ResultCh)
			p.active.Add(-1)
			cancelRead()
			<-readDone
			return true
		}
		// Capture the new channel immediately after StartRecognition resets it.
		// The server now has the entire duration of this job to send {"state":"listening"},
		// so the wait at the top of the next iteration is typically instant.
		listeningCh = cli.ListeningCh()

		jobDone := make(chan struct{})
		var jOnce sync.Once
		signalJobDone := func() { jOnce.Do(func() { close(jobDone) }) }

		// Wire callbacks for this job.
		cbMu.Lock()
		cbResult = func(text string, isFinal bool) {
			select {
			case job.ResultCh <- Result{Text: text, IsFinal: isFinal}:
			case <-jobDone:
			}
			if isFinal {
				signalJobDone()
			}
		}
		cbError = func(code int, msg string) {
			select {
			case job.ResultCh <- Result{ErrCode: code, ErrMsg: msg}:
			case <-jobDone:
			}
		}
		cbMu.Unlock()

		// Stream audio in background; StopRecognition when audioCh is drained.
		// audioDone is closed only after StopRecognition completes.
		audioDone := make(chan struct{})
		go func() {
			defer close(audioDone)
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
		case <-jobDone:
			// Wait for StopRecognition to complete before looping. Without this,
			// the next StartRecognition could race the in-flight stop on the wire.
			<-audioDone
			clearCallbacks(signalJobDone)
			p.completed.Add(1)
			close(job.ResultCh)
			p.active.Add(-1)
			p.idle.Add(1)

		case <-time.After(30 * time.Second):
			log.Printf("[broker] pool=%s  STT job timed out", p.cfg.Name)
			clearCallbacks(signalJobDone)
			p.errors.Add(1)
			close(job.ResultCh)
			p.active.Add(-1)
			cancelRead()
			<-readDone
			return true // treat timeout as drop; caller reconnects

		case err := <-connErrCh:
			log.Printf("[broker] pool=%s  STT connection dropped: %v", p.cfg.Name, err)
			clearCallbacks(signalJobDone)
			p.errors.Add(1)
			close(job.ResultCh)
			p.active.Add(-1)
			cancelRead()
			<-readDone
			return true

		case <-ctx.Done():
			clearCallbacks(signalJobDone)
			close(job.ResultCh)
			p.active.Add(-1)
			cancelRead()
			<-readDone
			return false
		}
	}
}

// ---- Routing ----

// Submit enqueues a job and returns routing metadata.
// Returns ErrServiceNotConfigured if no pool handles job.Service,
// ErrDraining if the broker is shutting down.
func (b *Broker) Submit(job Job) (SubmitResult, error) {
	if b.draining.Load() {
		return SubmitResult{}, ErrDraining
	}
	p, warning, err := b.resolvePool(job)
	if err != nil {
		return SubmitResult{}, err
	}
	p.queue.push(job)
	return SubmitResult{Pool: p.cfg.Name, Warning: warning}, nil
}

func (b *Broker) resolvePool(job Job) (*pool, string, error) {
	// Service existence check — applies regardless of pool tag.
	if len(b.services[job.Service]) == 0 {
		return nil, "", fmt.Errorf("%w: %q", ErrServiceNotConfigured, job.Service)
	}

	if job.Pool == "" {
		p, err := b.leastLoaded(job.Service)
		return p, "", err
	}

	named, ok := b.pools[job.Pool]
	if !ok {
		// Named pool not found — warn and fallback.
		p, err := b.leastLoaded(job.Service)
		if err != nil {
			return nil, "", err
		}
		w := fmt.Sprintf("pool %q not found, routed to %q", job.Pool, p.cfg.Name)
		log.Printf("[broker] %s", w)
		return p, w, nil
	}

	// Named pool found — check if congested.
	if isCongested(named) {
		p, err := b.leastLoaded(job.Service)
		if err != nil {
			return nil, "", err
		}
		if p.cfg.Name != named.cfg.Name {
			w := fmt.Sprintf("pool %q congested, routed to %q", job.Pool, p.cfg.Name)
			log.Printf("[broker] %s", w)
			return p, w, nil
		}
		// leastLoaded picked the same pool — accept without warning.
	}

	return named, "", nil
}

// isCongested returns true when all workers are active and the queue has backlog.
func isCongested(p *pool) bool {
	return p.active.Load() >= int32(p.cfg.Conns) && p.queue.len() > 0
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

// Metrics returns a snapshot of all pool counters for the metrics endpoint.
func (b *Broker) Metrics() []PoolMetrics {
	out := make([]PoolMetrics, 0, len(b.pools))
	for _, p := range b.pools {
		out = append(out, PoolMetrics{
			Name:      p.cfg.Name,
			Conns:     p.cfg.Conns,
			Active:    p.active.Load(),
			Idle:      p.idle.Load(),
			Queued:    p.queue.len(),
			Completed: p.completed.Load(),
			Errors:    p.errors.Load(),
		})
	}
	return out
}
