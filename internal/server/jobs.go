package server

// Detached reply jobs (Phase 5.7). A reply runs in its own goroutine, owned by a
// per-conversation job rather than the HTTP request, so it survives a client
// disconnect, a reload, or a tab close. A live client relays the job's SSE
// frames; after a disconnect (or a reload) the SPA re-attaches by polling the
// generation snapshot endpoint. Jobs are in memory only — nothing new is stored
// in the database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"myogchat/internal/store"
	"myogchat/internal/upstream"
)

// Job statuses (contract §3.7).
const (
	jobQueued  = "queued" // agent send waiting for the user's running agent job
	jobRunning = "running"
	jobDone    = "done"
	jobError   = "error"
	jobStopped = "stopped"
)

// Concurrency bounds and timing. The Pi is small and this is a personal app:
// keep a little headroom so a handful of concurrent replies is snappy without
// unbounded goroutines/upstream connections.
const (
	maxGlobalJobs = 8                // concurrent detached replies, process-wide
	maxUserJobs   = 3                // concurrent detached replies per user
	jobTimeout    = 15 * time.Minute // overall per-reply budget (tools included)
	jobGrace      = 60 * time.Second // keep a terminal job around for polling

	// Agent mode (per-conversation flag) gets a much bigger budget: 60 minutes
	// wall clock, 60 tool rounds and 10 minutes per upstream/tool call. One
	// agent job runs per user; further sends wait in a FIFO queue.
	agentJobTimeout  = 60 * time.Minute
	agentCallTimeout = 10 * time.Minute
	agentToolTimeout = 10 * time.Minute
	agentMaxSteps    = 60
)

// Errors returned by jobHub.start.
var (
	errJobRunning  = errors.New("a reply is already generating in this conversation")
	errTooManyJobs = errors.New("too many replies generating — wait for one to finish")
	// errShuttingDown is returned once the hub is draining: a job started now
	// would never be covered by ShutdownJobs' cancellation.
	errShuttingDown = errors.New("server is shutting down")
	// errAgentBusy is returned by startAgentExclusive when the user already has
	// an agent job. The calendar scheduler skips rather than queueing.
	errAgentBusy = errors.New("an agent job is already running for this user")
)

// sseFrame is one pre-rendered SSE frame (event name + marshaled JSON payload),
// fanned out to subscribers byte-for-byte.
type sseFrame struct {
	event string
	data  []byte
}

// newFrame marshals an SSE payload once. A marshal failure (impossible for the
// fixed shapes here) yields a frame with a JSON null body.
func newFrame(event string, data any) sseFrame {
	b, err := json.Marshal(data)
	if err != nil {
		b = []byte("null")
	}
	return sseFrame{event: event, data: b}
}

// jobSubscriber is one attached SSE client. Frames queue without bound (a reply
// is small) so a momentarily-slow writer can never stall the job.
type jobSubscriber struct {
	mu     sync.Mutex
	queue  []sseFrame
	closed bool
	signal chan struct{}
}

func newJobSubscriber() *jobSubscriber {
	return &jobSubscriber{signal: make(chan struct{}, 1)}
}

func (sub *jobSubscriber) push(f sseFrame) {
	sub.mu.Lock()
	if !sub.closed {
		sub.queue = append(sub.queue, f)
	}
	sub.mu.Unlock()
	sub.wake()
}

func (sub *jobSubscriber) close() {
	sub.mu.Lock()
	sub.closed = true
	sub.mu.Unlock()
	sub.wake()
}

// wake nudges the reader without blocking (a pending signal is enough).
func (sub *jobSubscriber) wake() {
	select {
	case sub.signal <- struct{}{}:
	default:
	}
}

// drain returns the queued frames and whether the subscriber has finished (its
// terminal frame, if any, is already in the returned slice).
func (sub *jobSubscriber) drain() ([]sseFrame, bool) {
	sub.mu.Lock()
	frames := sub.queue
	sub.queue = nil
	closed := sub.closed
	sub.mu.Unlock()
	return frames, closed
}

// genJob is one in-flight (or recently finished) assistant reply.
type genJob struct {
	id     string
	convID string
	userID string
	model  string
	// agent marks an agent-mode reply (bigger budget, workspace tools, the
	// one-per-user queue).
	agent  bool
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	status    string
	messageID string
	title     string
	// titleCh receives a title generated in parallel with the reply; nil when
	// the conversation already had one. Buffered so the writer never blocks.
	titleCh   chan string
	errMsg    string
	content   strings.Builder
	reasoning strings.Builder
	tools     []store.ToolActivity
	usage     *usageEvent
	step      int // 1-based tool round reported by the agent loop
	maxSteps  int
	subs      map[*jobSubscriber]struct{}

	done chan struct{}
}

// isRunning reports whether the job is still generating.
func (j *genJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == jobRunning
}

// failure returns the terminal error message, or "" when the job did not fail.
func (j *genJob) failure() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.errMsg
}

// emitLocked fans a frame out to every subscriber. Caller holds mu.
func (j *genJob) emitLocked(f sseFrame) {
	for sub := range j.subs {
		sub.push(f)
	}
}

// meta records the assistant message id and emits the first SSE frame.
func (j *genJob) meta(messageID string) {
	j.mu.Lock()
	j.messageID = messageID
	j.emitLocked(newFrame("meta", map[string]string{"message_id": messageID, "model": j.model}))
	j.mu.Unlock()
}

func (j *genJob) addText(delta string) {
	j.mu.Lock()
	j.content.WriteString(delta)
	j.emitLocked(newFrame("text", deltaEvent{Delta: delta}))
	j.mu.Unlock()
}

func (j *genJob) addReasoning(delta string) {
	j.mu.Lock()
	j.reasoning.WriteString(delta)
	j.emitLocked(newFrame("reasoning", deltaEvent{Delta: delta}))
	j.mu.Unlock()
}

// addTool folds one tool activity into the running list (same collapse the SSE
// frame and the persisted row use) and emits it.
func (j *genJob) addTool(t *upstream.ToolActivity) {
	entry := cappedTool(t)
	j.mu.Lock()
	j.tools = mergeToolEvent(j.tools, t)
	j.emitLocked(newFrame("tool", toolEvent{
		Name:       entry.Name,
		Status:     entry.Status,
		Summary:    entry.Summary,
		Args:       entry.Args,
		Output:     entry.Output,
		Error:      entry.Error,
		DurationMs: entry.DurationMs,
		Truncated:  entry.Truncated,
	}))
	j.mu.Unlock()
}

func (j *genJob) addUsage(ev usageEvent) {
	j.mu.Lock()
	j.usage = &ev
	j.emitLocked(newFrame("usage", ev))
	j.mu.Unlock()
}

// setStep records the tool loop's current round for the generation snapshot.
func (j *genJob) setStep(step, max int) {
	j.mu.Lock()
	j.step = step
	j.maxSteps = max
	j.mu.Unlock()
}

// finish marks the job terminal exactly once, emits the terminal frame (done or
// error; a stop ends the stream without a frame), closes subscribers, and
// releases the job context.
func (j *genJob) finish(status, title, errMsg string) {
	j.mu.Lock()
	if j.status != jobRunning {
		j.mu.Unlock()
		return
	}
	j.status = status
	j.title = title
	j.errMsg = errMsg
	switch status {
	case jobDone:
		j.emitLocked(newFrame("done", doneEvent{MessageID: j.messageID, Title: title}))
	case jobError:
		j.emitLocked(newFrame("error", errorEvent{Message: errMsg}))
	}
	for sub := range j.subs {
		sub.close()
	}
	j.subs = nil
	j.mu.Unlock()
	j.cancel()
	close(j.done)
}

// subscribe registers a new client and returns its subscriber. A job that is
// already terminal yields a closed subscriber (its terminal frame is queued).
func (j *genJob) subscribe() *jobSubscriber {
	sub := newJobSubscriber()
	j.mu.Lock()
	if j.status != jobRunning {
		sub.closed = true
	} else {
		if j.subs == nil {
			j.subs = make(map[*jobSubscriber]struct{})
		}
		j.subs[sub] = struct{}{}
	}
	j.mu.Unlock()
	if sub.closed {
		sub.wake()
	}
	return sub
}

func (j *genJob) unsubscribe(sub *jobSubscriber) {
	j.mu.Lock()
	delete(j.subs, sub)
	j.mu.Unlock()
	sub.close()
}

// agentProgress is the agent block of the generation snapshot. It is present
// only for an agent-mode conversation.
type agentProgress struct {
	Enabled       bool `json:"enabled"`
	Step          int  `json:"step"`
	MaxSteps      int  `json:"max_steps"`
	Queued        bool `json:"queued"`
	QueuePosition int  `json:"queue_position"`
}

// generationSnapshot is the owner-scoped view returned by
// GET /api/conversations/{id}/generation (contract §3.7). It carries enough to
// rehydrate the live bubble after a reload.
type generationSnapshot struct {
	ConversationID string               `json:"conversation_id"`
	Active         bool                 `json:"active"`
	Status         string               `json:"status"` // queued|running|done|error|stopped|none
	MessageID      string               `json:"message_id,omitempty"`
	Model          string               `json:"model,omitempty"`
	Title          string               `json:"title,omitempty"`
	Content        string               `json:"content,omitempty"`
	Reasoning      string               `json:"reasoning,omitempty"`
	Tools          []store.ToolActivity `json:"tools,omitempty"`
	Usage          *usageEvent          `json:"usage,omitempty"`
	Error          string               `json:"error,omitempty"`
	Agent          *agentProgress       `json:"agent,omitempty"`
}

func (j *genJob) snapshot() generationSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	snap := generationSnapshot{
		ConversationID: j.convID,
		Active:         j.status == jobRunning,
		Status:         j.status,
		MessageID:      j.messageID,
		Model:          j.model,
		Title:          j.title,
		Content:        j.content.String(),
		Reasoning:      j.reasoning.String(),
		Error:          j.errMsg,
	}
	if len(j.tools) > 0 {
		snap.Tools = append([]store.ToolActivity(nil), j.tools...)
	}
	if j.usage != nil {
		u := *j.usage
		snap.Usage = &u
	}
	if j.agent {
		snap.Agent = &agentProgress{Enabled: true, Step: j.step, MaxSteps: j.maxSteps}
	}
	return snap
}

// queuedReply is one agent send waiting for the user's running agent job. The
// API key is deliberately not held: it is resolved again at dequeue time.
type queuedReply struct {
	convID string
	userID string
	model  string
	spec   replySpec
}

// jobHub owns all jobs, keyed by conversation. One running job per conversation.
type jobHub struct {
	mu      sync.Mutex
	jobs    map[string]*genJob
	global  chan struct{}
	perUser map[string]int
	// queue holds agent sends waiting per user (FIFO, in memory only) and
	// agentActive counts each user's running agent job (at most 1).
	queue       map[string][]*queuedReply
	agentActive map[string]int
	// closing stops the queue from starting anything during shutdown.
	closing bool
}

func newJobHub() *jobHub {
	return &jobHub{
		jobs:        make(map[string]*genJob),
		global:      make(chan struct{}, maxGlobalJobs),
		perUser:     make(map[string]int),
		queue:       make(map[string][]*queuedReply),
		agentActive: make(map[string]int),
	}
}

// start reserves a slot and creates a job for convID. It refuses a second
// running job for the same conversation and enforces the global/per-user caps.
// The caller must launch runReply(j, …) and eventually call complete(j).
func (h *jobHub) start(convID, userID, model string, agent bool) (*genJob, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.jobs[convID]; ok && existing.isRunning() {
		return nil, errJobRunning
	}
	if h.queuedForLocked(convID) {
		return nil, errJobRunning
	}
	return h.startLocked(convID, userID, model, agent)
}

// queuedForLocked reports whether convID has a waiting agent send. Caller holds h.mu.
func (h *jobHub) queuedForLocked(convID string) bool {
	for _, q := range h.queue {
		for _, e := range q {
			if e.convID == convID {
				return true
			}
		}
	}
	return false
}

// startAgent starts an agent job for the user, or queues the reply when that
// user already has an agent job running. A non-zero pos is the 1-based queue
// position and j is nil. Caller holds no lock.
func (h *jobHub) startAgent(convID, userID, model string, spec replySpec) (j *genJob, pos int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		// A queued send would never be started once the hub is draining.
		return nil, 0, errShuttingDown
	}
	if existing, ok := h.jobs[convID]; ok && existing.isRunning() {
		return nil, 0, errJobRunning
	}
	if h.queuedForLocked(convID) {
		return nil, 0, errJobRunning
	}
	if h.agentActive[userID] > 0 {
		h.queue[userID] = append(h.queue[userID], &queuedReply{
			convID: convID, userID: userID, model: model, spec: spec,
		})
		return nil, len(h.queue[userID]), nil
	}
	j, err = h.startLocked(convID, userID, model, true)
	return j, 0, err
}

// startAgentExclusive starts an agent job for the user, or refuses: it never
// queues. The calendar scheduler uses it because a trigger that waits behind a
// 60-minute agent job would land after the event it prepares for. Caller holds
// no lock.
func (h *jobHub) startAgentExclusive(convID, userID, model string) (*genJob, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, errShuttingDown
	}
	if existing, ok := h.jobs[convID]; ok && existing.isRunning() {
		return nil, errJobRunning
	}
	if h.queuedForLocked(convID) {
		return nil, errJobRunning
	}
	if h.agentActive[userID] > 0 {
		return nil, errAgentBusy
	}
	return h.startLocked(convID, userID, model, true)
}

// startLocked reserves the concurrency slots and creates the job. Caller holds h.mu.
func (h *jobHub) startLocked(convID, userID, model string, agent bool) (*genJob, error) {
	// A job created after the drain began is not covered by ShutdownJobs'
	// cancellation, so it must never be created at all.
	if h.closing {
		return nil, errShuttingDown
	}
	select {
	case h.global <- struct{}{}:
	default:
		return nil, errTooManyJobs
	}
	if h.perUser[userID] >= maxUserJobs {
		<-h.global
		return nil, errTooManyJobs
	}
	h.perUser[userID]++
	if agent {
		h.agentActive[userID]++
	}

	timeout := jobTimeout
	maxSteps := 0
	if agent {
		timeout = agentJobTimeout
		maxSteps = agentMaxSteps
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	j := &genJob{
		id:       newUUID(),
		convID:   convID,
		userID:   userID,
		model:    model,
		agent:    agent,
		ctx:      ctx,
		cancel:   cancel,
		status:   jobRunning,
		maxSteps: maxSteps,
		done:     make(chan struct{}),
	}
	h.jobs[convID] = j
	return j, nil
}

// complete releases the concurrency slot for a finished job and schedules its
// removal after the grace window (so the snapshot endpoint can still answer a
// reload that races the terminal frame). When an agent job finishes, the user's
// next queued send is started and returned for the caller to launch. Safe to
// call once per job.
func (h *jobHub) complete(j *genJob) (*genJob, replySpec, bool) {
	h.mu.Lock()
	select {
	case <-h.global:
	default:
	}
	if h.perUser[j.userID] > 0 {
		h.perUser[j.userID]--
		if h.perUser[j.userID] == 0 {
			delete(h.perUser, j.userID)
		}
	}
	if j.agent && h.agentActive[j.userID] > 0 {
		h.agentActive[j.userID]--
		if h.agentActive[j.userID] <= 0 {
			delete(h.agentActive, j.userID)
		}
	}
	var (
		next     *genJob
		nextSpec replySpec
		ok       bool
	)
	if j.agent && !h.closing {
		next, nextSpec, ok = h.dequeueLocked(j.userID)
	}
	h.mu.Unlock()
	time.AfterFunc(jobGrace, func() {
		h.mu.Lock()
		if h.jobs[j.convID] == j {
			delete(h.jobs, j.convID)
		}
		h.mu.Unlock()
	})
	return next, nextSpec, ok
}

// dequeueLocked pops the user's next queued send and starts it. A send that
// cannot start (global cap) stays at the head of the queue for the next
// completion to pick up. Caller holds h.mu.
func (h *jobHub) dequeueLocked(userID string) (*genJob, replySpec, bool) {
	q := h.queue[userID]
	if len(q) == 0 {
		return nil, replySpec{}, false
	}
	j, err := h.startLocked(q[0].convID, q[0].userID, q[0].model, true)
	if err != nil {
		return nil, replySpec{}, false
	}
	spec := q[0].spec
	if len(q) > 1 {
		h.queue[userID] = q[1:]
	} else {
		delete(h.queue, userID)
	}
	return j, spec, true
}

// queuedPosition reports the 1-based queue position of a conversation's waiting
// send, or 0 when it is not queued.
func (h *jobHub) queuedPosition(convID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, q := range h.queue {
		for i, e := range q {
			if e.convID == convID {
				return i + 1
			}
		}
	}
	return 0
}

// removeQueued drops a conversation's waiting send. It reports whether one was
// removed.
func (h *jobHub) removeQueued(convID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for userID, q := range h.queue {
		for i, e := range q {
			if e.convID != convID {
				continue
			}
			if len(q) == 1 {
				delete(h.queue, userID)
			} else {
				h.queue[userID] = append(q[:i], q[i+1:]...)
			}
			return true
		}
	}
	return false
}

// get returns the job for a conversation, or nil.
func (h *jobHub) get(convID string) *genJob {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.jobs[convID]
}

// stop cancels the conversation's running job, or removes its queued send. It
// reports whether there was anything to stop.
func (h *jobHub) stop(convID string) bool {
	if h.removeQueued(convID) {
		return true
	}
	j := h.get(convID)
	if j == nil || !j.isRunning() {
		return false
	}
	j.cancel()
	return true
}

// shutdown cancels every running job and waits (bounded by ctx) for each to
// unwind. Queued agent sends are dropped, never started, and a canceled reply is
// treated like an explicit stop (an agent partial is persisted).
func (h *jobHub) shutdown(ctx context.Context) {
	h.mu.Lock()
	h.closing = true
	h.queue = make(map[string][]*queuedReply)
	jobs := make([]*genJob, 0, len(h.jobs))
	for _, j := range h.jobs {
		if j.isRunning() {
			jobs = append(jobs, j)
		}
	}
	h.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-ctx.Done():
			return
		}
	}
}

// ShutdownJobs cancels and drains detached reply jobs (called from main on
// graceful shutdown).
func (s *Server) ShutdownJobs(ctx context.Context) {
	s.hub().shutdown(ctx)
}

// handleGetGeneration returns the owner-scoped snapshot of a conversation's
// current (or recently finished) generation, for reattach/poll.
func (s *Server) handleGetGeneration(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	j := s.hub().get(conv.ID)
	if j == nil {
		// A queued agent send has no job yet: report its queue position so a
		// polling client keeps waiting instead of showing an idle chat.
		if pos := s.hub().queuedPosition(conv.ID); pos > 0 {
			writeJSON(w, http.StatusOK, generationSnapshot{
				ConversationID: conv.ID,
				Active:         true,
				Status:         jobQueued,
				Model:          conv.Model,
				Agent: &agentProgress{
					Enabled: true, MaxSteps: agentMaxSteps, Queued: true, QueuePosition: pos,
				},
			})
			return
		}
		writeJSON(w, http.StatusOK, generationSnapshot{
			ConversationID: conv.ID, Active: false, Status: "none",
		})
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot())
}

// handleStopGeneration cancels the running reply for a conversation.
func (s *Server) handleStopGeneration(w http.ResponseWriter, r *http.Request) {
	u, ok := currentUser(w, r)
	if !ok {
		return
	}
	conv, ok := s.getConversation(w, r, u.ID)
	if !ok {
		return
	}
	if !s.hub().stop(conv.ID) {
		writeError(w, http.StatusNotFound, "no reply is generating")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// relayJob streams a job's SSE frames to one client until the job terminates or
// the client disconnects. A disconnect only unsubscribes; the job keeps running.
func relayJob(w http.ResponseWriter, r *http.Request, flusher http.Flusher, j *genJob, sub *jobSubscriber) {
	defer j.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		frames, closed := sub.drain()
		for _, f := range frames {
			writeSSEFrame(w, flusher, f)
		}
		if closed {
			return
		}
		select {
		case <-sub.signal:
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err == nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			// Client disconnected: unsubscribe (deferred) and return. The job
			// continues and persists its reply.
			return
		}
	}
}

// writeSSEFrame writes one pre-rendered frame and flushes it.
func writeSSEFrame(w io.Writer, f http.Flusher, fr sseFrame) {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", fr.event, fr.data); err == nil {
		f.Flush()
	}
}
