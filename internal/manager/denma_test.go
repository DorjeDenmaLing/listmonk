package manager

// denma: the resume checkpoint (upstream PR #3222's tests, and the in-order
// checkpoint in denma.go), and failed sends and their retries.

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knadh/listmonk/models"
)

// mockStore records how the fetch cursor and the checkpoint are used.
type mockStore struct {
	mu               sync.Mutex
	batches          [][]models.Subscriber // returned by successive NextSubscribers calls
	gotCursors       []int                 // the cursors NextSubscribers was called with
	checkpointWrites []int                 // UpdateCampaignCounts' lastSubID

	failures   map[int]bool        // failed sends: subscriber ID -> temporary
	retries    []models.Subscriber // returned by the first DenmaRetrySubscribers call
	retryAll   []bool              // DenmaRetrySubscribers' all
	retrySent  []int               // DenmaRetrySent's subscribers
	retryLater bool                // DenmaRetryLater's answer
	statuses   []string            // UpdateCampaignStatus' statuses
}

func (s *mockStore) DenmaSendFailed(campID, subID int, reason string, temporary bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[int]bool{}
	}
	s.failures[subID] = temporary
	return nil
}
func (s *mockStore) DenmaRetrySent(campID, subID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retrySent = append(s.retrySent, subID)
	return nil
}
func (s *mockStore) DenmaRetrySubscribers(campID, afterID, limit int, all bool) ([]models.Subscriber, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryAll = append(s.retryAll, all)
	out := s.retries
	s.retries = nil
	return out, nil
}
func (s *mockStore) DenmaRetryLater(campID int) (bool, error) { return s.retryLater, nil }

func (s *mockStore) NextCampaigns(currentIDs []int64, sentCounts []int64, lastSubIDs []int64) ([]*models.Campaign, error) {
	return nil, nil
}

func (s *mockStore) NextSubscribers(campID, lastFetchedID, limit int) ([]models.Subscriber, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotCursors = append(s.gotCursors, lastFetchedID)
	if len(s.batches) == 0 {
		return nil, nil
	}
	out := s.batches[0]
	s.batches = s.batches[1:]
	return out, nil
}

func (s *mockStore) GetCampaign(campID int) (*models.Campaign, error) {
	return &models.Campaign{Status: models.CampaignStatusRunning}, nil
}
func (s *mockStore) GetAttachment(mediaID int) (models.Attachment, error) {
	return models.Attachment{}, nil
}
func (s *mockStore) GetInlineAttachmentByFilename(filename string) (models.Attachment, string, error) {
	return models.Attachment{}, "", nil
}
func (s *mockStore) UpdateCampaignStatus(campID int, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, status)
	return nil
}
func (s *mockStore) UpdateCampaignCounts(campID int, toSend int, sent int, lastSubID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpointWrites = append(s.checkpointWrites, lastSubID)
	return nil
}
func (s *mockStore) CreateLink(url string) (string, error) { return "", nil }
func (s *mockStore) BlocklistSubscriber(id int64) error    { return nil }
func (s *mockStore) DeleteSubscriber(id int64) error       { return nil }

// mockMessenger fails for the subscribers in fail and takes up to delay to
// send, so that concurrent workers finish out of order.
type mockMessenger struct {
	fail  map[int]bool
	codes map[int]int // SMTP replies to fail with
	delay time.Duration
}

func (m mockMessenger) Name() string { return "email" }
func (m mockMessenger) Push(msg models.Message) error {
	if m.delay > 0 {
		time.Sleep(time.Duration(rand.Int63n(int64(m.delay))))
	}
	if m.fail[msg.Subscriber.ID] {
		return errors.New("refused")
	}
	if c := m.codes[msg.Subscriber.ID]; c != 0 {
		return &textproto.Error{Code: c, Msg: "no"}
	}
	return nil
}
func (m mockMessenger) Flush() error { return nil }
func (m mockMessenger) Close() error { return nil }

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

func newTestManager(store Store, msgr Messenger) *Manager {
	return &Manager{
		cfg:        Config{BatchSize: 2, MessageRate: 1000, UnsubURL: "http://localhost:9000/subscription/%s/%s"},
		log:        log.New(discard{}, "", 0),
		fnNotify:   func(string, any) error { return nil },
		messengers: map[string]Messenger{"email": msgr},
		store:      store,
		pipes:      make(map[int]*pipe),
		links:      make(map[string]string),
		campMsgQ:   make(chan CampaignMessage, 100),
		msgQ:       make(chan models.Message, 100),
	}
}

func newTestCampaign(lastSubID int) *models.Campaign {
	c := &models.Campaign{
		UUID: "camp-uuid", Type: models.CampaignTypeRegular, Name: "test", Subject: "hi",
		FromEmail: "test@example.com", Body: "hello", Status: models.CampaignStatusRunning,
		ContentType: models.CampaignContentTypePlain, Messenger: "email", LastSubscriberID: lastSubID,
	}
	c.ID = 1
	return c
}

func makeSubs(ids ...int) []models.Subscriber {
	out := make([]models.Subscriber, 0, len(ids))
	for _, id := range ids {
		s := models.Subscriber{UUID: "sub-uuid", Email: "u@example.com", Name: "u"}
		s.ID = id
		out = append(out, s)
	}
	return out
}

// Fetching advances the pipe's cursor in memory and never writes the
// checkpoint.
func TestFetchCursorInMemory(t *testing.T) {
	store := &mockStore{batches: [][]models.Subscriber{makeSubs(1, 2), makeSubs(3, 4), {}}}
	p, err := newTestManager(store, mockMessenger{}).newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, false} {
		if has, err := p.NextSubscribers(); err != nil || has != want {
			t.Fatalf("fetch %d: has=%v err=%v, want %v", i, has, err, want)
		}
	}
	if got := store.gotCursors; len(got) != 3 || got[0] != 0 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("cursors %v, want [0 2 4]", got)
	}
	if len(store.checkpointWrites) != 0 {
		t.Fatalf("fetching wrote the checkpoint: %v", store.checkpointWrites)
	}
}

// A new pipe (after a restart) fetches from the saved checkpoint.
func TestResumeFromCheckpoint(t *testing.T) {
	store := &mockStore{batches: [][]models.Subscriber{makeSubs(8, 9)}}
	p, err := newTestManager(store, mockMessenger{}).newPipe(newTestCampaign(7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextSubscribers(); err != nil {
		t.Fatal(err)
	}
	if len(store.gotCursors) != 1 || store.gotCursors[0] != 7 {
		t.Fatalf("resumed at %v, want [7]", store.gotCursors)
	}
	if p.lastID.Load() != 7 {
		t.Fatalf("checkpoint %d, want 7", p.lastID.Load())
	}
}

// The campaign scan saves the checkpoint and, unlike the sent count, doesn't
// reset it.
func TestScanSavesCheckpoint(t *testing.T) {
	m := newTestManager(&mockStore{}, mockMessenger{})
	p, err := m.newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	p.sent.Store(3)
	p.lastID.Store(5)
	for i, want := range []int64{3, 0} {
		_, counts, lastIDs := m.getCurrentCampaigns()
		if counts[0] != want || lastIDs[0] != 5 {
			t.Fatalf("scan %d: count=%d lastID=%d, want %d and 5", i, counts[0], lastIDs[0], want)
		}
	}
}

// The checkpoint moves only past messages finished in order.
func TestCheckpointInOrder(t *testing.T) {
	p, err := newTestManager(&mockStore{}, mockMessenger{}).newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{10, 11, 12, 13, 14} {
		p.denmaQueued(id)
	}
	for _, step := range []struct{ finish, want int }{{11, 0}, {12, 0}, {10, 12}, {14, 12}, {13, 14}} {
		p.denmaFinished(step.finish)
		if got := int(p.lastID.Load()); got != step.want {
			t.Fatalf("after %d finished: checkpoint %d, want %d", step.finish, got, step.want)
		}
	}
}

// A message a worker skips (the campaign was paused) holds the checkpoint
// before it, so it's sent when the campaign resumes.
func TestCheckpointHoldsAtSkipped(t *testing.T) {
	p, err := newTestManager(&mockStore{}, mockMessenger{}).newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{1, 2, 3} {
		p.denmaQueued(id)
	}
	p.denmaFinished(1)
	p.denmaFinished(3) // 2 was skipped
	if got := p.lastID.Load(); got != 1 {
		t.Fatalf("checkpoint %d, want 1", got)
	}
}

// Through concurrent workers, sending out of order and with a failure: the
// checkpoint never passes an unfinished message, and ends at the last one.
func TestCheckpointThroughWorkers(t *testing.T) {
	var ids []int
	for i := 1; i <= 60; i++ {
		ids = append(ids, i)
	}
	store := &mockStore{batches: [][]models.Subscriber{makeSubs(ids[:20]...), makeSubs(ids[20:40]...), makeSubs(ids[40:]...)}}
	m := newTestManager(store, mockMessenger{fail: map[int]bool{33: true}, delay: 3 * time.Millisecond})
	p, err := m.newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}

	// Record every message's finish, and check the checkpoint against them.
	var (
		mu       sync.Mutex
		finished = map[int]bool{}
		bad      []string
	)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A message is recorded as finished before its worker moves the
			// checkpoint, so every ID up to the checkpoint must be recorded.
			mu.Lock()
			cp := int(p.lastID.Load())
			for id := 1; id <= cp; id++ {
				if !finished[id] {
					bad = append(bad, fmt.Sprintf("checkpoint at %d with %d unfinished", cp, id))
					break
				}
			}
			mu.Unlock()
			time.Sleep(200 * time.Microsecond)
		}
	}()
	m.messengers["email"] = recorder{m.messengers["email"], func(id int) {
		mu.Lock()
		finished[id] = true
		mu.Unlock()
	}}

	for w := 0; w < 4; w++ {
		go m.worker()
	}
	for {
		has, err := p.NextSubscribers()
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			break
		}
	}
	p.wg.Done() // the pipe's own count (newPipe), as the manager does when fetching ends
	deadline := time.Now().Add(5 * time.Second)
	for p.lastID.Load() != 60 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	if got := p.lastID.Load(); got != 60 {
		t.Fatalf("checkpoint %d at the end, want 60", got)
	}
	if len(bad) > 0 {
		t.Fatal(bad[0])
	}
	if s := p.sent.Load(); s != 59 {
		t.Fatalf("sent %d, want 59 (one failed)", s)
	}
}

// recorder notes each message as it finishes (before the worker records it).
type recorder struct {
	Messenger
	fn func(int)
}

func (r recorder) Push(msg models.Message) error {
	err := r.Messenger.Push(msg)
	r.fn(msg.Subscriber.ID)
	return err
}

// A stopped campaign (paused for errors, or a shutdown) fetches no more.
func TestStoppedFetchesNothing(t *testing.T) {
	store := &mockStore{batches: [][]models.Subscriber{makeSubs(1, 2)}}
	p, err := newTestManager(store, mockMessenger{}).newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	p.Stop(false)
	if has, err := p.NextSubscribers(); has || err != nil || len(store.gotCursors) != 0 {
		t.Fatalf("has=%v err=%v fetches=%d, want none", has, err, len(store.gotCursors))
	}
}

// sendAll runs a pipe to the end of its subscribers through n workers, as
// the manager does, and waits for cleanup().
func sendAll(t *testing.T, m *Manager, p *pipe, n int) {
	t.Helper()
	for w := 0; w < n; w++ {
		go m.worker()
	}
	for {
		has, err := p.NextSubscribers()
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			break
		}
	}
	p.wg.Done()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.pipesMut.RLock()
		_, running := m.pipes[p.camp.ID]
		m.pipesMut.RUnlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the pipe didn't end")
}

// Failed sends are recorded, as temporary unless the server refused for good.
func TestFailuresRecorded(t *testing.T) {
	store := &mockStore{batches: [][]models.Subscriber{makeSubs(1, 2), makeSubs(3, 4)}}
	m := newTestManager(store, mockMessenger{codes: map[int]int{2: 454, 3: 554}, fail: map[int]bool{4: true}})
	m.cfg.MaxSendErrors = 1000
	p, err := m.newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	sendAll(t, m, p, 2)
	if got := fmt.Sprint(store.failures); got != "map[2:true 3:false 4:true]" {
		t.Fatalf("failures %s, want 2 and 4 temporary, 3 not", got)
	}
	if fmt.Sprint(store.statuses) != "[finished]" {
		t.Fatalf("statuses %v, want [finished]", store.statuses)
	}
}

// A run of failures pauses the campaign; one sent in between starts the
// count again.
func TestFailStreakPauses(t *testing.T) {
	ids := func(from, to int) []int {
		var out []int
		for i := from; i <= to; i++ {
			out = append(out, i)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		ok    int // the one that's sent, 0 for none
		pause bool
	}{{"all fail", 0, true}, {"one sent at 16", 16, false}} {
		fail := map[int]bool{}
		for _, id := range ids(1, 30) {
			fail[id] = id != tc.ok
		}
		var batches [][]models.Subscriber
		for i := 1; i <= 30; i += 2 {
			batches = append(batches, makeSubs(i, i+1))
		}
		store := &mockStore{batches: batches}
		m := newTestManager(store, mockMessenger{fail: fail})
		m.cfg.MaxSendErrors = 1000
		p, err := m.newPipe(newTestCampaign(0))
		if err != nil {
			t.Fatal(err)
		}
		sendAll(t, m, p, 1)
		paused := fmt.Sprint(store.statuses) == "[paused]"
		if paused != tc.pause || p.denmaStreakHit.Load() != tc.pause {
			t.Fatalf("%s: statuses %v, want paused=%v", tc.name, store.statuses, tc.pause)
		}
		if tc.pause && !strings.HasPrefix(p.denmaPauseReason(), "20 sends in a row failed") {
			t.Fatalf("%s: pause reason %q", tc.name, p.denmaPauseReason())
		}
		if tc.pause && (len(store.failures) < 20 || len(store.failures) > 22) {
			t.Fatalf("%s: %d failures recorded, want about 20 (the rest skipped)", tc.name, len(store.failures))
		}
	}
	if r := (&pipe{m: &Manager{cfg: Config{MaxSendErrors: 5}}}).denmaStreakLimit(); r != 5 {
		t.Fatalf("limit %d with max_send_errors 5, want 5", r)
	}
	if r := (&pipe{m: &Manager{cfg: Config{MaxSendErrors: 0}}}).denmaStreakLimit(); r != 0 {
		t.Fatalf("limit %d with max_send_errors off, want none", r)
	}
}

// A pipe sends the failed sends again first (all of them, on a resumed
// campaign), then the rest, skipping those.
func TestRetriesFirst(t *testing.T) {
	store := &mockStore{retries: makeSubs(3, 7), batches: [][]models.Subscriber{makeSubs(3, 4), makeSubs(7, 8)}}
	m := newTestManager(store, mockMessenger{})
	p, err := m.newPipe(newTestCampaign(2))
	if err != nil {
		t.Fatal(err)
	}
	for {
		if has, err := p.NextSubscribers(); err != nil {
			t.Fatal(err)
		} else if !has {
			break
		}
	}
	var got []string
	for len(m.campMsgQ) > 0 {
		msg := <-m.campMsgQ
		got = append(got, fmt.Sprintf("%d%s", msg.Subscriber.ID, map[bool]string{true: "r"}[msg.denmaRetry]))
	}
	if fmt.Sprint(got) != "[3r 7r 4 8]" {
		t.Fatalf("queued %v, want [3r 7r 4 8]", got)
	}
	if fmt.Sprint(store.retryAll) != "[true true]" {
		t.Fatalf("retry fetches %v, want all of them", store.retryAll)
	}
}

// A retry run sends only the temporary failures, and nothing else.
func TestRetryRunOnly(t *testing.T) {
	store := &mockStore{retries: makeSubs(3), batches: [][]models.Subscriber{makeSubs(9)}}
	m := newTestManager(store, mockMessenger{})
	c := newTestCampaign(8)
	c.DenmaRetryAt.Valid = true
	p, err := m.newPipe(c)
	if err != nil {
		t.Fatal(err)
	}
	sendAll(t, m, p, 1)
	if len(store.gotCursors) != 0 || fmt.Sprint(store.retryAll) != "[false false]" {
		t.Fatalf("main fetches %v, retry fetches %v; want none, and temporary ones only", store.gotCursors, store.retryAll)
	}
	if fmt.Sprint(store.retrySent) != "[3]" || p.sent.Load() != 1 {
		t.Fatalf("retries sent %v (sent %d), want [3]", store.retrySent, p.sent.Load())
	}
	if p.lastID.Load() != 8 {
		t.Fatalf("checkpoint %d, want 8 (a retry doesn't move it)", p.lastID.Load())
	}
}

// A campaign with failed sends to try again later stays running.
func TestCleanupWaitsForRetries(t *testing.T) {
	store := &mockStore{retryLater: true, batches: [][]models.Subscriber{makeSubs(1)}}
	m := newTestManager(store, mockMessenger{codes: map[int]int{1: 421}})
	p, err := m.newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	sendAll(t, m, p, 1)
	if len(store.statuses) != 0 || len(store.checkpointWrites) != 1 {
		t.Fatalf("statuses %v, counts saved %d times; want it left running, its counts saved", store.statuses, len(store.checkpointWrites))
	}
}

// A campaign message waiting for the daily limit is skipped once the campaign
// stops, and the checkpoint stays before it.
func TestDailyWaitSkips(t *testing.T) {
	stopped := make(chan struct{})
	DenmaDailyWait = func(stop func() bool) bool {
		for !stop() {
			time.Sleep(time.Millisecond)
		}
		close(stopped)
		return false
	}
	defer func() { DenmaDailyWait = nil }()
	store := &mockStore{batches: [][]models.Subscriber{makeSubs(1)}}
	m := newTestManager(store, mockMessenger{})
	p, err := m.newPipe(newTestCampaign(0))
	if err != nil {
		t.Fatal(err)
	}
	go m.worker()
	if _, err := p.NextSubscribers(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	p.Stop(false)
	<-stopped
	p.wg.Done()
	time.Sleep(20 * time.Millisecond)
	if p.sent.Load() != 0 || p.lastID.Load() != 0 || len(store.failures) != 0 {
		t.Fatalf("sent %d, checkpoint %d, failures %v; want it skipped, not sent or failed", p.sent.Load(), p.lastID.Load(), store.failures)
	}
}
