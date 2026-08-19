package cli

import (
	"container/list"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/fetch"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
)

// jobState is a job's observable lifecycle position.
//
// These four values exist so a caller can tell "not finished yet" from
// "this job died" from "I have never heard of this id". Returning the
// bare ResultEnvelope instead would collapse all three into one
// indistinguishable 404, and the difference between a job still running
// and a job that dead-lettered is the whole point of the read side: one
// is a measured outcome, the other is a failure to measure.
type jobState string

const (
	stateQueued       jobState = "queued"
	stateProcessing   jobState = "processing"
	stateDone         jobState = "done"
	stateDeadLettered jobState = "dead_lettered"
	// stateNotFound is reachable ONLY through the batch form. The
	// single-job GET says "never existed, or evicted" with HTTP 404, but
	// one 200 answering about many ids cannot carry a status code per
	// entry — and omitting the entry would collapse "unknown id" into "we
	// did not answer", the same mistake as the bare envelope. So the
	// batch form spells the 404 out as a state.
	stateNotFound jobState = "not_found"
)

// jobStatus is the status document both read routes return.
//
// Result is present ONLY for stateDone, and it is the envelope the
// configured sinks received for the same job_id — not a copy, not a
// summary. Reason is present ONLY for stateDeadLettered. A dead-lettered
// job produced no envelope anyone can stand behind, so this type must
// never synthesize one: a manufactured verdict:"error" result would put a
// decision on the wire that no pipeline run ever made.
type jobStatus struct {
	JobID  queue.JobID            `json:"job_id"`
	State  jobState               `json:"state"`
	Result *result.ResultEnvelope `json:"result,omitempty"`
	Reason string                 `json:"reason,omitempty"`
}

// stateRank orders the lifecycle so a transition can only move forward.
//
// Two races make this necessary rather than decorative. The intake
// handler records "queued" after Enqueue returns, by which time a worker
// may already have finished the job; and at-least-once delivery
// (redisq) can redeliver a job that already reached a terminal state.
// Without the ordering, either would walk a finished job backwards to
// "queued" or "processing" and a poller would wait forever on a job that
// is already answered. Done and dead-lettered share the top rank: the
// first terminal state observed is the one that stands.
func stateRank(s jobState) int {
	switch s {
	case stateQueued:
		return 0
	case stateProcessing:
		return 1
	default:
		return 2
	}
}

// resultStore is the read side's bounded, ephemeral job-state cache: an
// LRU capped at maxEntries, with a per-record TTL.
//
// It is NOT durable and it is NOT the audit log. A restart empties it, an
// eviction is silent, and an expiry is silent — the sinks and the
// hash-chained audit log remain the record of what was decided. Its whole
// job is to answer "where is this job right now" inside a stated horizon.
//
// Every method tolerates a nil receiver, because a nil store is exactly
// what intake.result_api.enabled=false produces: the routes are never
// registered, no memory is allocated for verdicts nobody can read, and
// the call sites in the worker handler stay unconditional.
type resultStore struct {
	mu  sync.Mutex
	max int
	ttl time.Duration
	// now is the clock, injectable so the TTL is tested by moving time
	// rather than by sleeping through it.
	now func() time.Time
	lru *list.List // front = most recently touched
	idx map[queue.JobID]*list.Element
}

type storeEntry struct {
	doc     jobStatus
	expires time.Time
}

func newResultStore(maxEntries int, ttl time.Duration) *resultStore {
	return &resultStore{
		max: maxEntries,
		ttl: ttl,
		now: time.Now,
		lru: list.New(),
		idx: make(map[queue.JobID]*list.Element, maxEntries),
	}
}

// record stores a transition, ignoring any that would move a job
// backwards. An expired record is replaced outright: it is already
// unreadable, so nothing is being walked back.
func (s *resultStore) record(doc jobStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if el, ok := s.idx[doc.JobID]; ok {
		e := el.Value.(*storeEntry)
		if e.live(now) && stateRank(doc.State) <= stateRank(e.doc.State) {
			return
		}
		e.doc, e.expires = doc, now.Add(s.ttl)
		s.lru.MoveToFront(el)
		return
	}
	s.idx[doc.JobID] = s.lru.PushFront(&storeEntry{doc: doc, expires: now.Add(s.ttl)})
	for s.lru.Len() > s.max {
		s.drop(s.lru.Back())
	}
}

// get returns the status document for id. An evicted or expired record is
// indistinguishable from one that never existed, and both answer "not
// found" — never a soft "unknown but probably fine" that a caller could
// read as benign (invariant 1).
func (s *resultStore) get(id queue.JobID) (jobStatus, bool) {
	if s == nil {
		return jobStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.idx[id]
	if !ok {
		return jobStatus{}, false
	}
	e := el.Value.(*storeEntry)
	if !e.live(s.now()) {
		s.drop(el)
		return jobStatus{}, false
	}
	s.lru.MoveToFront(el)
	return e.doc, true
}

// len reports how many records are held, expired ones included: expiry is
// lazy, so this is the number the max_entries cap actually bounds.
func (s *resultStore) len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lru.Len()
}

// drop removes one element. Callers hold s.mu.
func (s *resultStore) drop(el *list.Element) {
	delete(s.idx, el.Value.(*storeEntry).doc.JobID)
	s.lru.Remove(el)
}

func (e *storeEntry) live(now time.Time) bool { return e.expires.After(now) }

// Env var names for the read side's basic-auth credentials. They are
// resolved through config.Secret() (invariant 4) and named literally here
// only so the 503 body can tell an operator what to set.
const (
	envResultAPIUser = config.EnvPrefix + "_RESULT_API_USER"
	envResultAPIPass = config.EnvPrefix + "_RESULT_API_PASSWORD"
)

// maxBatchIDs caps POST /jobs/status. The store itself is bounded, so an
// unbounded request list buys a caller nothing except a way to make one
// request hold the mutex the worker handler needs for every transition.
const maxBatchIDs = 1000

// resultAPI serves the read side. It holds no queue, no pipeline and no
// config mutation seam: every route it registers is a read.
type resultAPI struct {
	cfg   config.ResultAPIConfig
	store *resultStore
	user  string
	pass  string
}

func newResultAPI(cfg config.ResultAPIConfig, store *resultStore) *resultAPI {
	secret := config.Secret()
	return &resultAPI{
		cfg:   cfg,
		store: store,
		// "result_api.user" resolves to VISMOD_RESULT_API_USER; yaml can
		// never supply either (config.refuseYAMLCredentials).
		user: secret("result_api.user"),
		pass: secret("result_api.password"),
	}
}

// auth mirrors ui.Server.auth exactly, including the realm and the 503.
// Two surfaces with the same posture must fail the same way, or the one
// an operator tests is not the one that protects them.
func (a *resultAPI) auth(next http.HandlerFunc) http.HandlerFunc {
	if a.cfg.Auth == config.AuthModeNone {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if a.user == "" || a.pass == "" {
			// Never open. An endpoint that discloses verdicts because its
			// credentials were not set is worse than one that is down.
			http.Error(w, "result api auth is 'basic' but "+
				envResultAPIUser+"/"+envResultAPIPass+" are not set",
				http.StatusServiceUnavailable)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(a.user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(a.pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="vismod"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// route is one registered pattern and its handler. The read side builds
// its routes as data rather than as a sequence of mux.HandleFunc calls so
// that the set is enumerable — http.ServeMux does not expose what it
// holds, and "no mutating route was added here" is a property worth being
// able to assert rather than to review by eye.
type route struct {
	pattern string
	handler http.HandlerFunc
}

// resultAPIRoutes returns the read-side routes, or nil when the API is
// off. Nil is the whole disabled behavior: no route, no store, no
// disclosure.
func resultAPIRoutes(cfg config.ResultAPIConfig, store *resultStore) []route {
	if !cfg.Enabled || store == nil {
		return nil
	}
	a := newResultAPI(cfg, store)
	return []route{
		{"GET /jobs/{id}", a.auth(a.getJob)},
		{"POST /jobs/status", a.auth(a.batchStatus)},
	}
}

func (a *resultAPI) getJob(w http.ResponseWriter, r *http.Request) {
	doc, ok := a.store.get(queue.JobID(r.PathValue("id")))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, doc)
}

// batchStatusRequest is the POST /jobs/status body.
type batchStatusRequest struct {
	JobIDs []queue.JobID `json:"job_ids"`
}

type batchStatusResponse struct {
	Jobs []jobStatus `json:"jobs"`
}

func (a *resultAPI) batchStatus(w http.ResponseWriter, r *http.Request) {
	var req batchStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.JobIDs) == 0 {
		http.Error(w, `bad request: job_ids is required — {"job_ids":["job-1",...]}`, http.StatusBadRequest)
		return
	}
	if len(req.JobIDs) > maxBatchIDs {
		http.Error(w, "bad request: at most "+strconv.Itoa(maxBatchIDs)+" job_ids per request", http.StatusBadRequest)
		return
	}
	// One document per REQUESTED id, in the order asked, duplicates
	// included: a caller must be able to zip the response back onto its
	// own list without matching on anything.
	resp := batchStatusResponse{Jobs: make([]jobStatus, 0, len(req.JobIDs))}
	for _, id := range req.JobIDs {
		if doc, ok := a.store.get(id); ok {
			resp.Jobs = append(resp.Jobs, doc)
			continue
		}
		resp.Jobs = append(resp.Jobs, jobStatus{JobID: id, State: stateNotFound})
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// recordTerminalStatus writes a job's final observed state to the read
// side. The caller has already excluded queue.Retry: a job waiting on a
// retry is still in flight, and its state has not changed.
func recordTerminalStatus(s *resultStore, id queue.JobID, env result.ResultEnvelope, disp queue.Disposition, perr error) {
	if s == nil {
		return
	}
	if disp == queue.Ack {
		// The envelope, verbatim — the same value the configured sinks
		// received for this job_id, not a summary of it.
		done := env
		s.record(jobStatus{JobID: id, State: stateDone, Result: &done})
		return
	}
	// Deliberately no result, even though a dead-lettered job usually DOES
	// have a verdict:"error" envelope by this point. Attaching it would
	// make every response the same shape at the cost of the one thing this
	// state is for: a dead-lettered job is a job whose outcome nobody
	// should act on, and a result key is an invitation to act on it.
	s.record(jobStatus{JobID: id, State: stateDeadLettered, Reason: deadLetterReason(perr)})
}

// deadLetterReason renders the same text the driver writes onto the
// queue.DeadLetterEntry for a handler-returned dead-letter (memq.process
// and redisq both format it this way), so the status document and the DLQ
// entry for one job agree rather than telling two stories. It is redacted
// because a reason is free text built from the pipeline's error.
func deadLetterReason(perr error) string {
	return redactReason(fmt.Sprintf("dead-lettered: %v", perr))
}

// urlInText finds anything URL-shaped inside a free-text error string.
var urlInText = regexp.MustCompile(`https?://[^\s"'<>\\]+`)

// redactReason strips the query string from every URL in a dead-letter
// reason.
//
// A reason is free text built from the pipeline's error, and a url job's
// error can be a *url.Error — whose message quotes the request URL in
// full, query string included. queue.Job.Source.Ref holds the PRESIGNED
// url for exactly as long as the fetcher needs it, so an unredacted
// reason on this endpoint would hand a credential to every caller who can
// read a job's status. fetch.Redact is the same reduction the envelope's
// own Source gets, applied here to text rather than to a field.
func redactReason(s string) string {
	return urlInText.ReplaceAllStringFunc(s, func(u string) string {
		red, _ := fetch.Redact(u)
		return red
	})
}
