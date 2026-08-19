package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vismod/vismod/internal/config"
	"github.com/vismod/vismod/internal/queue"
	"github.com/vismod/vismod/internal/result"
	"github.com/vismod/vismod/pkg/moderation"
)

// readAPIConfig is intakeConfig with the read side on and auth off. Auth
// is exercised on its own below; the routing and shape tests would
// otherwise assert nothing but the 401 path.
func readAPIConfig() config.Config {
	c := intakeConfig()
	c.Intake.ResultAPI = config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeNone, MaxEntries: 100, TTL: time.Hour,
	}
	return c
}

// readIntakeOver builds an intake handler serving the read side from an
// EXISTING store, so a test can drive states in and read documents out
// through the same mux the write path uses.
func readIntakeOver(t *testing.T, c config.Config, q queue.Queue, st *resultStore) http.Handler {
	t.Helper()
	srv := serveIntake(c, q, openBackpressure(), &intakeSwitch{}, testLogger(), withResultStore(st))
	if srv == nil {
		t.Fatal("serveIntake returned nil for a configured intake_addr")
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv.Handler
}

func newReadIntake(t *testing.T, c config.Config, q queue.Queue) (http.Handler, *resultStore) {
	t.Helper()
	st := newResultStore(c.Intake.ResultAPI.MaxEntries, c.Intake.ResultAPI.TTL)
	return readIntakeOver(t, c, q, st), st
}

func getStatus(h http.Handler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/jobs/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postBatch(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/jobs/status", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeDoc decodes a status document into a map, so a test can assert on
// the KEYS that reached the wire (a struct would silently accept a missing
// one, and "result is absent" is a load-bearing property here).
func decodeDoc(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status document: %v (body %q)", err, rec.Body.String())
	}
	return doc
}

// doneEnvelope is a minimal envelope for store-level tests. End-to-end
// tests below use the real pipeline's envelope instead.
func doneEnvelope(id queue.JobID) result.ResultEnvelope {
	return result.ResultEnvelope{
		JobID:  id,
		Source: moderation.Source{Kind: "file", Ref: "/tmp/" + string(id) + ".jpg", MediaType: "image"},
	}
}

// repoFile reads a file from the repository root. Two acceptance criteria
// are about what an operator is TOLD (auth "none" is loopback-only; the
// store is not the audit log), and a promise that lives only in a code
// comment is a promise nobody making the decision will ever read.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestJobStatusDocumentShapeAndRouting: the read route lives on the SAME
// mux that POST /jobs does — one listener, one address to firewall — and
// the document it returns is {job_id, state} with result/reason present
// only when they mean something.
func TestJobStatusDocumentShapeAndRouting(t *testing.T) {
	q := testMemq(t)
	h, st := newReadIntake(t, readAPIConfig(), q)

	// Submitted and read back through one handler: this is the routing
	// assertion. A second mux would fail here, not in review.
	rec := post(h, `{"kind":"file","ref":"in.jpg","media_type":"image"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /jobs = %d, want 202: %s", rec.Code, rec.Body)
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("POST /jobs returned no job_id: %v %s", err, rec.Body)
	}

	got := getStatus(h, accepted.JobID)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /jobs/%s = %d, want 200", accepted.JobID, got.Code)
	}
	if ct := got.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	doc := decodeDoc(t, got)
	if doc["job_id"] != accepted.JobID {
		t.Errorf("job_id = %v, want the id POST /jobs handed out (%s)", doc["job_id"], accepted.JobID)
	}
	if _, ok := doc["result"]; ok {
		t.Error("a job that has not finished carries a result key")
	}
	if _, ok := doc["reason"]; ok {
		t.Error("a job that has not dead-lettered carries a reason key")
	}

	// Every state this document can report is one of the four, and each
	// one renders the keys that state is allowed to carry.
	for _, tc := range []struct {
		doc        jobStatus
		wantResult bool
		wantReason bool
	}{
		{jobStatus{JobID: "s-q", State: stateQueued}, false, false},
		{jobStatus{JobID: "s-p", State: stateProcessing}, false, false},
		{jobStatus{JobID: "s-d", State: stateDone, Result: ptrEnvelope(doneEnvelope("s-d"))}, true, false},
		{jobStatus{JobID: "s-x", State: stateDeadLettered, Reason: "dead-lettered: boom"}, false, true},
	} {
		st.record(tc.doc)
		body := decodeDoc(t, getStatus(h, string(tc.doc.JobID)))
		if body["state"] != string(tc.doc.State) {
			t.Errorf("state = %v, want %q", body["state"], tc.doc.State)
		}
		if _, ok := body["result"]; ok != tc.wantResult {
			t.Errorf("state %q: result key present = %v, want %v", tc.doc.State, ok, tc.wantResult)
		}
		if _, ok := body["reason"]; ok != tc.wantReason {
			t.Errorf("state %q: reason key present = %v, want %v", tc.doc.State, ok, tc.wantReason)
		}
	}
}

func ptrEnvelope(e result.ResultEnvelope) *result.ResultEnvelope { return &e }

// TestJobStatusDistinguishesAllFiveStates is the reason this endpoint
// returns a status document rather than the bare envelope. All five
// outcomes must be tellable apart from each other: three of them would
// otherwise be the same 404, and a harness reading that endpoint would
// count a job that is merely still running as missing — a run failure —
// when the same 404 might mean the job dead-lettered (a measured
// abstention) or was never submitted at all.
func TestJobStatusDistinguishesAllFiveStates(t *testing.T) {
	h, st := newReadIntake(t, readAPIConfig(), testMemq(t))

	st.record(jobStatus{JobID: "five-queued", State: stateQueued})
	st.record(jobStatus{JobID: "five-processing", State: stateProcessing})
	st.record(jobStatus{JobID: "five-done", State: stateDone, Result: ptrEnvelope(doneEnvelope("five-done"))})
	st.record(jobStatus{JobID: "five-dead", State: stateDeadLettered, Reason: "dead-lettered: verdict=error: unscorable"})

	for _, tc := range []struct {
		id    string
		code  int
		state string
	}{
		{"five-queued", http.StatusOK, "queued"},
		{"five-processing", http.StatusOK, "processing"},
		{"five-done", http.StatusOK, "done"},
		{"five-dead", http.StatusOK, "dead_lettered"},
		{"five-never-submitted", http.StatusNotFound, ""},
	} {
		rec := getStatus(h, tc.id)
		if rec.Code != tc.code {
			t.Fatalf("GET /jobs/%s = %d, want %d", tc.id, rec.Code, tc.code)
		}
		if tc.state == "" {
			continue
		}
		if got := decodeDoc(t, rec)["state"]; got != tc.state {
			t.Errorf("GET /jobs/%s state = %v, want %q", tc.id, got, tc.state)
		}
	}

	// The distinction is only real if the four 200s differ from each
	// other. Equal bodies would pass every check above and still collapse.
	seen := map[string]string{}
	for _, id := range []string{"five-queued", "five-processing", "five-done", "five-dead"} {
		body := getStatus(h, id).Body.String()
		state, _ := decodeDoc(t, getStatus(h, id))["state"].(string)
		if prev, dup := seen[state]; dup {
			t.Errorf("state %q reported for two different jobs (%s and %s)", state, prev, id)
		}
		seen[state] = id
		if strings.Contains(body, "not_found") {
			t.Errorf("GET /jobs/%s leaked the batch-only not_found state: %s", id, body)
		}
	}
	if len(seen) != 4 {
		t.Errorf("four stored jobs produced %d distinct states: %v", len(seen), seen)
	}
}

// TestDoneStateCarriesTheSameEnvelopeTheSinkGot: the read side is a second
// view of one decision, not a second decision. If the document's result
// and the sink's line could differ, an operator reconciling an incident
// would have two records and no way to tell which one the verdict was
// made from.
func TestDoneStateCarriesTheSameEnvelopeTheSinkGot(t *testing.T) {
	c := serveConfig(t)
	c.Intake.ResultAPI = config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeNone, MaxEntries: 10, TTL: time.Hour,
	}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()
	if s.results == nil {
		t.Fatal("intake.result_api.enabled=true allocated no store")
	}

	input := writeInput(t, "bad.jpg", "BLOCK")
	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID:          "readback-done-1",
		Source:      moderation.Source{Kind: "file", Ref: input, MediaType: "image"},
		Metadata:    json.RawMessage(`{"case_id":"abc-123"}`),
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runServer(t, s)
	sinkLine := waitForFile(t, sinkPath(c))
	stop()

	h := readIntakeOver(t, c, s.queue, s.results)
	rec := getStatus(h, "readback-done-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /jobs/readback-done-1 = %d, want 200: %s", rec.Code, rec.Body)
	}
	var doc struct {
		State  string          `json:"state"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.State != "done" {
		t.Fatalf("state = %q, want done", doc.State)
	}

	// Compared as decoded JSON rather than as text: the sink and the
	// endpoint marshal the same struct, so any difference is a difference
	// in the VALUE, which is the thing that must not drift.
	var fromAPI, fromSink any
	if err := json.Unmarshal(doc.Result, &fromAPI); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(sinkLine)), &fromSink); err != nil {
		t.Fatalf("decode sink line: %v", err)
	}
	gotJSON, _ := json.Marshal(fromAPI)
	wantJSON, _ := json.Marshal(fromSink)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("the read side and the sink disagree about job readback-done-1:\n api  = %s\n sink = %s", gotJSON, wantJSON)
	}
	if !strings.Contains(string(gotJSON), `"verdict":"block"`) {
		t.Errorf("the block verdict never reached the read side: %s", gotJSON)
	}
}

// TestBatchStatusReturnsOneDocumentPerRequestedID: a harness polling one
// id per request would need one round trip per case. The batch form must
// answer about every id it was asked about — including the ones the store
// does not hold — in the order asked, so the caller can zip the response
// back onto its own list without matching on anything.
func TestBatchStatusReturnsOneDocumentPerRequestedID(t *testing.T) {
	h, st := newReadIntake(t, readAPIConfig(), testMemq(t))
	st.record(jobStatus{JobID: "batch-done", State: stateDone, Result: ptrEnvelope(doneEnvelope("batch-done"))})
	st.record(jobStatus{JobID: "batch-dead", State: stateDeadLettered, Reason: "dead-lettered: boom"})

	rec := postBatch(h, `{"job_ids":["batch-done","batch-unknown","batch-dead","batch-done"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /jobs/status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []struct{ id, state string }{
		{"batch-done", "done"},
		{"batch-unknown", "not_found"},
		{"batch-dead", "dead_lettered"},
		{"batch-done", "done"},
	}
	if len(resp.Jobs) != len(want) {
		t.Fatalf("got %d documents for %d requested ids: %s", len(resp.Jobs), len(want), rec.Body)
	}
	for i, w := range want {
		if resp.Jobs[i]["job_id"] != w.id || resp.Jobs[i]["state"] != w.state {
			t.Errorf("jobs[%d] = %v, want job_id %q state %q", i, resp.Jobs[i], w.id, w.state)
		}
	}
	if _, ok := resp.Jobs[1]["result"]; ok {
		t.Error("an unknown id came back with a result")
	}

	// Bounded input, both ways: an id list is caller-controlled.
	if rec := postBatch(h, `{"job_ids":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty job_ids = %d, want 400 — an empty answer reads as 'all unknown'", rec.Code)
	}
	if rec := postBatch(h, `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", rec.Code)
	}
	ids := make([]string, maxBatchIDs+1)
	for i := range ids {
		ids[i] = `"x"`
	}
	if rec := postBatch(h, `{"job_ids":[`+strings.Join(ids, ",")+`]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized job_ids = %d, want 400", rec.Code)
	}
}

// TestResultAPIBasicAuthMirrorsUI: this endpoint discloses verdicts, refs
// and caller metadata. It gets the UI's auth, unchanged — same realm, same
// constant-time comparison, same refusal — because two surfaces with the
// same posture must fail the same way, or the one an operator tests is not
// the one that protects them.
func TestResultAPIBasicAuthMirrorsUI(t *testing.T) {
	t.Setenv(envResultAPIUser, "operator")
	t.Setenv(envResultAPIPass, "correct-horse")

	c := readAPIConfig()
	c.Intake.ResultAPI.Auth = config.AuthModeBasic
	h, st := newReadIntake(t, c, testMemq(t))
	st.record(jobStatus{JobID: "auth-job", State: stateQueued})

	for _, tc := range []struct {
		name       string
		user, pass string
		send       bool
		want       int
	}{
		{"correct credentials", "operator", "correct-horse", true, http.StatusOK},
		{"wrong password", "operator", "wrong", true, http.StatusUnauthorized},
		{"wrong user", "intruder", "correct-horse", true, http.StatusUnauthorized},
		{"no credentials", "", "", false, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/jobs/auth-job", nil)
			if tc.send {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("GET /jobs/auth-job = %d, want %d", rec.Code, tc.want)
			}
			if tc.want != http.StatusUnauthorized {
				return
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="vismod"` {
				t.Errorf("WWW-Authenticate = %q, want the UI's realm", got)
			}
		})
	}

	// The batch route is behind the same wrapper, not a second copy of it.
	req := httptest.NewRequest(http.MethodPost, "/jobs/status", strings.NewReader(`{"job_ids":["auth-job"]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /jobs/status = %d, want 401", rec.Code)
	}

	// A behavioral test cannot observe a timing-safe comparison, and a
	// byte-by-byte == would pass every case above. The comparison itself
	// is therefore asserted at the source, where it can regress.
	src := repoFile(t, "internal/cli/resultapi.go")
	for _, want := range []string{
		"subtle.ConstantTimeCompare([]byte(u), []byte(a.user)) != 1",
		"subtle.ConstantTimeCompare([]byte(p), []byte(a.pass)) != 1",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the result api no longer compares credentials in constant time: %q is gone", want)
		}
	}

	// POST /jobs must NOT have grown auth: it is the unchanged write path.
	if rec := post(h, `{"kind":"file","ref":"in.jpg","media_type":"image"}`); rec.Code != http.StatusAccepted {
		t.Errorf("POST /jobs = %d with the read side authenticated, want 202 — the write path is unchanged", rec.Code)
	}
}

// TestResultAPIAuthNonePermittedAndDocumentedLoopbackOnly: "none" is
// permitted because a loopback-bound intake behind a sidecar has no
// credential to hold. That is only safe if the operator choosing it is
// told the condition, so the docs carry the constraint the code cannot.
func TestResultAPIAuthNonePermittedAndDocumentedLoopbackOnly(t *testing.T) {
	h, st := newReadIntake(t, readAPIConfig(), testMemq(t)) // auth: none
	st.record(jobStatus{JobID: "open-job", State: stateQueued})

	rec := getStatus(h, "open-job") // no Authorization header at all
	if rec.Code != http.StatusOK {
		t.Fatalf("auth 'none' returned %d for an unauthenticated read, want 200", rec.Code)
	}
	if got := decodeDoc(t, rec)["state"]; got != "queued" {
		t.Errorf("state = %v, want queued", got)
	}
	if err := config.Validate(withResultAPI(config.Defaults(), config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeNone, MaxEntries: 10, TTL: time.Minute,
	})); err != nil {
		t.Errorf("auth 'none' must be a permitted mode, got %v", err)
	}

	for _, doc := range []string{"config.example.yaml", "docs/rest-api.md"} {
		text := strings.ToLower(repoFile(t, doc))
		if !strings.Contains(text, "loopback") {
			t.Errorf(`%s never says "loopback" — an operator choosing auth: "none" is not told the condition that makes it safe`, doc)
		}
		if !strings.Contains(text, "result_api") {
			t.Errorf("%s does not document intake.result_api at all", doc)
		}
	}
}

func withResultAPI(c config.Config, r config.ResultAPIConfig) config.Config {
	c.Intake.ResultAPI = r
	return c
}

// TestStoreEvictsByLRUAndTTL: the store is bounded and ephemeral on
// purpose. Both horizons must actually bind — an LRU that grows past its
// cap is a memory leak in the worker, and a TTL that never fires turns a
// read-side convenience into an unmanaged second copy of every verdict.
func TestStoreEvictsByLRUAndTTL(t *testing.T) {
	st := newResultStore(3, time.Hour)
	for _, id := range []queue.JobID{"e1", "e2", "e3"} {
		st.record(jobStatus{JobID: id, State: stateQueued})
	}
	// Touching e1 makes e2 the least recently used, so the next insert
	// must evict e2 rather than the oldest INSERT.
	if _, ok := st.get("e1"); !ok {
		t.Fatal("e1 vanished before any eviction was due")
	}
	st.record(jobStatus{JobID: "e4", State: stateQueued})

	if st.len() != 3 {
		t.Errorf("store holds %d entries, want max_entries = 3", st.len())
	}
	if _, ok := st.get("e2"); ok {
		t.Error("the least recently used entry survived eviction")
	}
	for _, id := range []queue.JobID{"e1", "e3", "e4"} {
		if _, ok := st.get(id); !ok {
			t.Errorf("%s was evicted; only the least recently used one should be", id)
		}
	}

	// An evicted entry is a 404 over HTTP, never a soft "unknown but
	// probably fine" a caller could read as benign (invariant 1).
	h := readIntakeOver(t, readAPIConfig(), testMemq(t), st)
	if rec := getStatus(h, "e2"); rec.Code != http.StatusNotFound {
		t.Errorf("GET an evicted id = %d, want 404", rec.Code)
	}

	// TTL, on a clock the test moves rather than one it waits for.
	now := time.Now()
	ttl := newResultStore(10, time.Minute)
	ttl.now = func() time.Time { return now }
	ttl.record(jobStatus{JobID: "t1", State: stateDone, Result: ptrEnvelope(doneEnvelope("t1"))})
	if _, ok := ttl.get("t1"); !ok {
		t.Fatal("a record expired inside its own TTL")
	}
	now = now.Add(time.Minute + time.Second)
	if _, ok := ttl.get("t1"); ok {
		t.Error("a record outlived its ttl")
	}
	if ttl.len() != 0 {
		t.Errorf("an expired record was answered as absent but still held: len = %d", ttl.len())
	}
	// An expired id is writable again: nothing is being walked backwards
	// once the record it would contradict is already unreadable.
	now = now.Add(time.Second)
	ttl.record(jobStatus{JobID: "t1", State: stateQueued})
	if doc, ok := ttl.get("t1"); !ok || doc.State != stateQueued {
		t.Errorf("re-recording an expired id gave %+v, %v", doc, ok)
	}
}

// TestStoreTransitionsOnlyMoveForward: at-least-once delivery redelivers
// finished jobs, and the intake records "queued" after Enqueue has
// already handed the job to a worker. Either would walk a finished job
// backwards, and a poller waiting on a terminal state would then wait
// forever on a job that was answered minutes ago.
func TestStoreTransitionsOnlyMoveForward(t *testing.T) {
	st := newResultStore(10, time.Hour)
	st.record(jobStatus{JobID: "m1", State: stateProcessing})
	st.record(jobStatus{JobID: "m1", State: stateDone, Result: ptrEnvelope(doneEnvelope("m1"))})
	for _, back := range []jobState{stateQueued, stateProcessing} {
		st.record(jobStatus{JobID: "m1", State: back})
		if doc, _ := st.get("m1"); doc.State != stateDone {
			t.Fatalf("a %q transition overwrote a finished job: state = %q", back, doc.State)
		}
	}
	// Terminal is terminal: the first one observed stands, so a redelivery
	// cannot rewrite a decision that was already published.
	st.record(jobStatus{JobID: "m1", State: stateDeadLettered, Reason: "redelivered"})
	if doc, _ := st.get("m1"); doc.State != stateDone || doc.Reason != "" {
		t.Errorf("a redelivery rewrote a terminal state: %+v", doc)
	}
	// A nil store is the disabled configuration; every call must be inert.
	var nilStore *resultStore
	nilStore.record(jobStatus{JobID: "m1", State: stateDone})
	if _, ok := nilStore.get("m1"); ok || nilStore.len() != 0 {
		t.Error("a nil store answered a read")
	}
}

// TestResultStoreUnderConcurrentTransitionsAndReads: in a running worker
// this store is written by every worker goroutine and by the intake
// handler, and read by every HTTP request, all at once. One mutex covers
// the map and the LRU list together because they must not disagree — a
// list holding an element the map has already dropped is an eviction that
// never frees anything.
//
// -race cannot run on this box (CGO_ENABLED=0), so this is a looped
// concurrent exercise, not a race gate. See docs/agent/UNVERIFIED.md.
func TestResultStoreUnderConcurrentTransitionsAndReads(t *testing.T) {
	const workers, jobs = 8, 60
	st := newResultStore(16, time.Hour) // deliberately smaller than the job count
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < jobs; i++ {
				id := queue.JobID("c-" + strconv.Itoa(i))
				st.record(jobStatus{JobID: id, State: stateQueued})
				st.record(jobStatus{JobID: id, State: stateProcessing})
				st.record(jobStatus{JobID: id, State: stateDone, Result: ptrEnvelope(doneEnvelope(id))})
				st.get(queue.JobID("c-" + strconv.Itoa((i+w)%jobs)))
			}
		}(w)
	}
	wg.Wait()

	if got := st.len(); got > 16 {
		t.Errorf("store holds %d entries under concurrent writes, want at most max_entries = 16", got)
	}
	// The map and the list have to agree, or eviction leaks: every id the
	// map still knows must be readable, and reading them all must not
	// change the count.
	before := st.len()
	live := 0
	for i := 0; i < jobs; i++ {
		if doc, ok := st.get(queue.JobID("c-" + strconv.Itoa(i))); ok {
			live++
			if doc.State != stateDone {
				t.Errorf("c-%d survived as %q; every job reached done", i, doc.State)
			}
		}
	}
	if live != before || st.len() != before {
		t.Errorf("map and LRU disagree: %d readable, %d held before, %d after", live, before, st.len())
	}
}

// TestDisabledResultAPIIs404AndAllocatesNoStore: off by default is the
// whole security posture. "Enabled but empty" is not off — it still holds
// every verdict in memory, waiting for a misconfiguration to expose it —
// so the assertion is on the nil store, not only on the status code.
func TestDisabledResultAPIIs404AndAllocatesNoStore(t *testing.T) {
	if def := config.Defaults().Intake.ResultAPI; def.Enabled {
		t.Error("intake.result_api.enabled defaults to true; the read side must be opt-in")
	}

	s, err := newServer(serveConfig(t)) // serveConfig inherits the defaults
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()
	if s.results != nil {
		t.Error("the disabled read side allocated a store")
	}

	h := readIntakeOver(t, intakeConfig(), testMemq(t), s.results)
	if rec := getStatus(h, "anything"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /jobs/{id} = %d with the read side off, want 404", rec.Code)
	}
	if rec := postBatch(h, `{"job_ids":["anything"]}`); rec.Code != http.StatusNotFound {
		t.Errorf("POST /jobs/status = %d with the read side off, want 404", rec.Code)
	}
	if routes := resultAPIRoutes(config.Defaults().Intake.ResultAPI, newResultStore(1, time.Hour)); routes != nil {
		t.Errorf("the disabled read side registered %d routes", len(routes))
	}
	// Enabled with no store is the same refusal: a route that cannot
	// answer must not exist rather than answer softly.
	if routes := resultAPIRoutes(readAPIConfig().Intake.ResultAPI, nil); routes != nil {
		t.Errorf("the read side registered %d routes with no store behind them", len(routes))
	}
	// The write path is untouched by any of this.
	if rec := post(h, `{"kind":"file","ref":"in.jpg","media_type":"image"}`); rec.Code != http.StatusAccepted {
		t.Errorf("POST /jobs = %d with the read side off, want 202", rec.Code)
	}
}

// TestDeadLetteredJobReturnsReasonAndNoResult: a dead-lettered job
// produced no decision anyone should act on. Synthesizing a
// verdict:"error" result to make every response the same shape would put
// a verdict on the wire that no pipeline run ever made — and attaching
// the error envelope that does exist is an invitation to act on it.
func TestDeadLetteredJobReturnsReasonAndNoResult(t *testing.T) {
	c := serveConfig(t)
	c.Intake.ResultAPI = config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeNone, MaxEntries: 10, TTL: time.Hour,
	}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	input := writeInput(t, "boom.jpg", "ERROR") // the scripted provider fails
	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID:          "readback-dead-1",
		Source:      moderation.Source{Kind: "file", Ref: input, MediaType: "image"},
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runServer(t, s)
	waitForFile(t, sinkPath(c)) // the error envelope still reaches the sink
	stop()

	h := readIntakeOver(t, c, s.queue, s.results)
	rec := getStatus(h, "readback-dead-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /jobs/readback-dead-1 = %d, want 200: %s", rec.Code, rec.Body)
	}
	doc := decodeDoc(t, rec)
	if doc["state"] != "dead_lettered" {
		t.Fatalf("state = %v, want dead_lettered", doc["state"])
	}
	if _, ok := doc["result"]; ok {
		t.Errorf("a dead-lettered job came back carrying a result: %s", rec.Body)
	}
	reason, _ := doc["reason"].(string)
	if reason == "" {
		t.Fatal("a dead-lettered job came back with no reason; the caller cannot tell why it died")
	}
	// The same text the driver writes onto the queue.DeadLetterEntry, so
	// the status document and the DLQ tell one story about one job.
	if !strings.HasPrefix(reason, "dead-lettered: ") {
		t.Errorf("reason = %q, want the queue's dead-letter wording", reason)
	}
	if !strings.Contains(reason, "verdict=error") {
		t.Errorf("reason = %q, want the pipeline's cause", reason)
	}
	if strings.Contains(rec.Body.String(), `"verdict"`) {
		t.Errorf("a verdict was synthesized onto a dead-lettered job: %s", rec.Body)
	}
}

// TestResultAPIBasicWithUnsetCredentialsReturns503: never open. An
// endpoint that discloses verdicts because nobody set its credentials is
// worse than one that is down, and the 503 has to name the variables or
// the operator has no way to act on it.
func TestResultAPIBasicWithUnsetCredentialsReturns503(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{
		{"both unset", "", ""},
		{"user unset", "", "correct-horse"},
		{"password unset", "operator", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envResultAPIUser, tc.user)
			t.Setenv(envResultAPIPass, tc.pass)

			c := readAPIConfig()
			c.Intake.ResultAPI.Auth = config.AuthModeBasic
			h, st := newReadIntake(t, c, testMemq(t))
			st.record(jobStatus{JobID: "unset-job", State: stateDone, Result: ptrEnvelope(doneEnvelope("unset-job"))})

			req := httptest.NewRequest(http.MethodGet, "/jobs/unset-job", nil)
			req.SetBasicAuth("operator", "correct-horse")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET = %d, want 503; basic auth with no credentials must never serve", rec.Code)
			}
			for _, name := range []string{envResultAPIUser, envResultAPIPass} {
				if !strings.Contains(rec.Body.String(), name) {
					t.Errorf("the 503 body does not name %s: %s", name, rec.Body)
				}
			}
			if strings.Contains(rec.Body.String(), "unset-job") {
				t.Error("the refusal disclosed a job id")
			}
		})
	}
}

// TestResultAPIResponseNeverCarriesMediaRawOrSecret: this endpoint is a
// NEW transmission surface (invariant 3) — an envelope now reaches a
// network caller who is not a configured sink. Everything the envelope
// deliberately keeps out of band must stay out of band here too.
func TestResultAPIResponseNeverCarriesMediaRawOrSecret(t *testing.T) {
	const mediaMarker = "MEDIA-BYTES-b7f2c1"
	t.Setenv(envResultAPIUser, "operator")
	t.Setenv(envResultAPIPass, "s3cr3t-do-not-disclose")

	c := serveConfig(t)
	c.Intake.ResultAPI = config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeBasic, MaxEntries: 10, TTL: time.Hour,
	}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	// "BLOCK" drives the verdict; the rest of the file is the media the
	// response must never echo.
	input := writeInput(t, "bad.jpg", "BLOCK "+mediaMarker)
	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID:          "readback-leak-1",
		Source:      moderation.Source{Kind: "file", Ref: input, MediaType: "image"},
		Metadata:    json.RawMessage(`{"case_id":"abc-123"}`),
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	stop := runServer(t, s)
	waitForFile(t, sinkPath(c))
	stop()

	h := readIntakeOver(t, c, s.queue, s.results)
	req := httptest.NewRequest(http.MethodGet, "/jobs/readback-leak-1", nil)
	req.SetBasicAuth("operator", "s3cr3t-do-not-disclose")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()

	for _, forbidden := range []struct{ what, needle string }{
		{"media bytes", mediaMarker},
		{"the provider raw payload", `"raw"`},
		{"the audit-only raw digest", "raw_sha256"},
		{"the basic-auth password", "s3cr3t-do-not-disclose"},
		{"the basic-auth user", `"operator"`},
	} {
		if strings.Contains(body, forbidden.needle) {
			t.Errorf("the response discloses %s (%q): %s", forbidden.what, forbidden.needle, body)
		}
	}
	// The batch form serializes the same document, so it inherits the
	// same boundary rather than being a second one.
	breq := httptest.NewRequest(http.MethodPost, "/jobs/status", strings.NewReader(`{"job_ids":["readback-leak-1"]}`))
	breq.SetBasicAuth("operator", "s3cr3t-do-not-disclose")
	brec := httptest.NewRecorder()
	h.ServeHTTP(brec, breq)
	if strings.Contains(brec.Body.String(), mediaMarker) || strings.Contains(brec.Body.String(), "raw_sha256") {
		t.Errorf("the batch form discloses what the single form does not: %s", brec.Body)
	}
	// Caller metadata IS disclosed, by design and by documentation. The
	// test pins it so the docs' "do not put secrets in metadata" warning
	// stays true rather than cautious.
	if !strings.Contains(body, `"case_id":"abc-123"`) {
		t.Errorf("caller metadata did not survive the read path: %s", body)
	}
}

// TestResultAPINeverDisclosesThePresignedURL: a url job's queue payload
// holds the FULL presigned url for as long as the fetcher needs it, while
// everything recorded carries the redacted form. A read-side response is
// now one of the things "recorded" reaches.
//
// The job under test dead-letters, and that is the point: with no
// credentials and no reachable host, a url job cannot reach "done" in
// this suite — and the dead-lettered document is the leaky one anyway,
// because its reason is free text built from an error, and a *url.Error
// quotes the request url in full.
func TestResultAPINeverDisclosesThePresignedURL(t *testing.T) {
	const signature = "X-Amz-Signature=deadbeef"
	c := serveConfig(t)
	c.Source.URL.AllowHosts = []string{"media.example.com"}
	c.Intake.ResultAPI = config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeNone, MaxEntries: 10, TTL: time.Hour,
	}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()

	if _, err := s.queue.Enqueue(context.Background(), queue.Job{
		ID: "readback-url-1",
		Source: moderation.Source{
			Kind:      "url",
			Ref:       "https://media.example.com/clip.mp4?" + signature,
			MediaType: "image",
		},
		SubmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runServer(t, s)
	deadline := time.Now().Add(15 * time.Second)
	var doc jobStatus
	for time.Now().Before(deadline) {
		if d, ok := s.results.get("readback-url-1"); ok && d.State != stateQueued && d.State != stateProcessing {
			doc = d
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if doc.State == "" {
		t.Fatal("the url job never reached a terminal state")
	}

	h := readIntakeOver(t, c, s.queue, s.results)
	bodies := map[string]string{
		"GET /jobs/{id}":    getStatus(h, "readback-url-1").Body.String(),
		"POST /jobs/status": postBatch(h, `{"job_ids":["readback-url-1"]}`).Body.String(),
	}
	for route, body := range bodies {
		for _, needle := range []string{signature, "deadbeef", "X-Amz"} {
			if strings.Contains(body, needle) {
				t.Errorf("%s published the presigned credential (%q): %s", route, needle, body)
			}
		}
		if !strings.Contains(body, "media.example.com/clip.mp4") {
			t.Errorf("%s dropped the ref entirely rather than redacting it — the caller cannot tell which asset died: %s", route, body)
		}
	}
}

// redactReason is the reduction those bodies rely on, tested directly
// because the shape of a *url.Error is not something this suite controls.
func TestRedactReasonStripsQueryStringsFromFreeText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`dead-lettered: Get "https://h/a.mp4?sig=abc": dial tcp: refused`,
			`dead-lettered: Get "https://h/a.mp4": dial tcp: refused`},
		{"fetch: after 3 attempts: http://h/x?k=v and https://h2/y?k=v2 both failed",
			"fetch: after 3 attempts: http://h/x and https://h2/y both failed"},
		{"dead-lettered: verdict=error: unreadable input", "dead-lettered: verdict=error: unreadable input"},
		// Userinfo is a credential too, and fetch.Redact drops it.
		{"fetch: https://user:pw@h/a.mp4 failed", "fetch: https://h/a.mp4 failed"},
		// Unparseable: fetch.Redact returns the empty string, so the ref
		// disappears rather than being passed through unredacted. Losing
		// the ref is the right way to fail here.
		{"fetch: https://h/%zz?sig=abc failed", "fetch:  failed"},
		{"", ""},
	} {
		if got := redactReason(tc.in); got != tc.want {
			t.Errorf("redactReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestResultAPIAddsNoMutatingRoute: the read side is read-only, and that
// is a property worth asserting rather than reviewing by eye. http.ServeMux
// does not expose what it holds, so the routes are built as data and the
// test enumerates that data.
func TestResultAPIAddsNoMutatingRoute(t *testing.T) {
	routes := resultAPIRoutes(readAPIConfig().Intake.ResultAPI, newResultStore(4, time.Hour))
	if len(routes) != 2 {
		t.Fatalf("the read side registered %d routes, want exactly 2", len(routes))
	}
	for _, rt := range routes {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			t.Fatalf("route %q names no method; a method-less pattern answers every verb", rt.pattern)
		}
		switch method {
		case http.MethodGet:
		case http.MethodPost:
			// The one POST is a read: a batch GET that needs a body.
			if path != "/jobs/status" {
				t.Errorf("route %q is a POST to something other than the batch read", rt.pattern)
			}
		default:
			t.Errorf("route %q uses %s — the read side adds no mutating verb", rt.pattern, method)
		}
	}

	// And nothing mutating answers on the live mux either.
	h, st := newReadIntake(t, readAPIConfig(), testMemq(t))
	st.record(jobStatus{JobID: "ro-job", State: stateDone, Result: ptrEnvelope(doneEnvelope("ro-job"))})
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch} {
		for _, path := range []string{"/jobs/ro-job", "/jobs/status", "/jobs"} {
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code < 400 {
				t.Errorf("%s %s = %d; the read side must not have opened a mutating route", method, path, rec.Code)
			}
		}
	}
	// The job is still exactly where it was: nothing above changed it.
	if doc, ok := st.get("ro-job"); !ok || doc.State != stateDone {
		t.Errorf("a mutating request changed a job's state: %+v %v", doc, ok)
	}
}

// TestPostJobsAndBootOrderUnchangedByResultAPI: the read side is additive.
// Turning it on must not move boot, must not change what POST /jobs
// answers, and must not change whether the worker drains.
func TestPostJobsAndBootOrderUnchangedByResultAPI(t *testing.T) {
	body := `{"kind":"file","ref":"in.jpg","media_type":"image"}`
	off := post(newIntake(t, intakeConfig(), testMemq(t), openBackpressure(), &intakeSwitch{}), body)
	on, _ := func() (*httptest.ResponseRecorder, *resultStore) {
		h, st := newReadIntake(t, readAPIConfig(), testMemq(t))
		return post(h, body), st
	}()
	if off.Code != on.Code || off.Code != http.StatusAccepted {
		t.Fatalf("POST /jobs = %d with the read side off and %d with it on, want 202 both", off.Code, on.Code)
	}
	var offDoc, onDoc map[string]any
	if err := json.Unmarshal(off.Body.Bytes(), &offDoc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal(on.Body.Bytes(), &onDoc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(offDoc) != len(onDoc) || (offDoc["job_id"] == nil) != (onDoc["job_id"] == nil) {
		t.Errorf("POST /jobs changed shape: %v vs %v", offDoc, onDoc)
	}

	// Rejections are unchanged too: a bad request must not start being
	// recorded, or a caller could read state for a job that never existed.
	for _, bad := range []string{`{"kind":"file"}`, `{"kind":"kafka","ref":"x"}`, `{`} {
		h, st := newReadIntake(t, readAPIConfig(), testMemq(t))
		if rec := post(h, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("POST /jobs %s = %d, want 400", bad, rec.Code)
		}
		if st.len() != 0 {
			t.Errorf("a rejected POST /jobs %s left %d records behind", bad, st.len())
		}
	}

	// Boot and drain, with the read side on, from the same entry points
	// the existing serve tests use.
	c := serveConfig(t)
	c.Intake.ResultAPI = config.ResultAPIConfig{
		Enabled: true, Auth: config.AuthModeNone, MaxEntries: 10, TTL: time.Hour,
	}
	s, err := newServer(c)
	if err != nil {
		t.Fatalf("newServer with the read side enabled: %v", err)
	}
	defer s.close()
	if s.results == nil || s.queue == nil || s.pipeline == nil || s.tracker == nil {
		t.Fatal("enabling the read side changed what boot produces")
	}
	stop := runServer(t, s)
	stop()
}
