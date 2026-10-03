package manager

// denma: the resume checkpoint (upstream PR #3222's tests, and the in-order
// checkpoint in denma.go).

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
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
}

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

func (s *mockStore) GetCampaign(campID int) (*models.Campaign, error) { return &models.Campaign{}, nil }
func (s *mockStore) GetAttachment(mediaID int) (models.Attachment, error) {
	return models.Attachment{}, nil
}
func (s *mockStore) GetInlineAttachmentByFilename(filename string) (models.Attachment, string, error) {
	return models.Attachment{}, "", nil
}
func (s *mockStore) UpdateCampaignStatus(campID int, status string) error { return nil }
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
