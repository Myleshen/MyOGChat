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
)

// Errors returned by jobHub.start.
var (
	errJobRunning  = errors.New("a reply is already generating in this conversation")
	errTooManyJobs = errors.New("too many replies generating — wait for one to finish")
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
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	status    string
	messageID string
	title     string
	errMsg    string
	content   strings.Builder
	reasoning strings.Builder
	tools     []store.ToolActivity
	usage     *usageEvent
	subs      map[*jobSubscriber]struct{}

	done chan struct{}
}

// isRunning reports whether the job is still generating.
func (j *genJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == jobRunning
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

// generationSnapshot is the owner-scoped view returned by
// GET /api/conversations/{id}/generation (contract §3.7). It carries enough to
// rehydrate the live bubble after a reload.
type generationSnapshot struct {
	ConversationID string               `json:"conversation_id"`
	Active         bool                 `json:"active"`
	Status         string               `json:"status"` // running|done|error|stopped|none
	MessageID      string               `json:"message_id,omitempty"`
	Model          string               `json:"model,omitempty"`
	Title          string               `json:"title,omitempty"`
	Content        string               `json:"content,omitempty"`
	Reasoning      string               `json:"reasoning,omitempty"`
	Tools          []store.ToolActivity `json:"tools,omitempty"`
	Usage          *usageEvent          `json:"usage,omitempty"`
	Error          string               `json:"error,omitempty"`
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
	return snap
}

// jobHub owns all jobs, keyed by conversation. One running job per conversation.
type jobHub struct {
	mu      sync.Mutex
	jobs    map[string]*genJob
	global  chan struct{}
	perUser map[string]int
}

func newJobHub() *jobHub {
	return &jobHub{
		jobs:    make(map[string]*genJob),
		global:  make(chan struct{}, maxGlobalJobs),
		perUser: make(map[string]int),
	}
}

// start reserves a slot and creates a job for convID. It refuses a second
// running job for the same conversation and enforces the global/per-user caps.
// The caller must launch runReply(j, …) and eventually call complete(j).
func (h *jobHub) start(convID, userID, model string) (*genJob, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.jobs[convID]; ok && existing.isRunning() {
		return nil, errJobRunning
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

	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	j := &genJob{
		id:     newUUID(),
		convID: convID,
		userID: userID,
		model:  model,
		ctx:    ctx,
		cancel: cancel,
		status: jobRunning,
		done:   make(chan struct{}),
	}
	h.jobs[convID] = j
	return j, nil
}

// complete releases the concurrency slot for a finished job and schedules its
// removal after the grace window (so the snapshot endpoint can still answer a
// reload that races the terminal frame). Safe to call once per job.
func (h *jobHub) complete(j *genJob) {
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
	h.mu.Unlock()
	time.AfterFunc(jobGrace, func() {
		h.mu.Lock()
		if h.jobs[j.convID] == j {
			delete(h.jobs, j.convID)
		}
		h.mu.Unlock()
	})
}

// get returns the job for a conversation, or nil.
func (h *jobHub) get(convID string) *genJob {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.jobs[convID]
}

// stop cancels the conversation's running job. It reports whether one was
// running.
func (h *jobHub) stop(convID string) bool {
	j := h.get(convID)
	if j == nil || !j.isRunning() {
		return false
	}
	j.cancel()
	return true
}

// shutdown cancels every running job and waits (bounded by ctx) for each to
// unwind. A canceled reply is treated like an explicit stop — partial output is
// not persisted.
func (h *jobHub) shutdown(ctx context.Context) {
	h.mu.Lock()
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
