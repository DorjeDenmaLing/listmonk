package denmadaily

// The daily sending limit's count.

import (
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// counter returns a counter with n e-mails sent minsAgo minutes ago, for each
// pair.
func counter(limit int, sent ...[2]int) *Counter {
	d := New(log.New(os.Stderr, "", 0))
	d.limit = limit
	now := time.Now().Unix() / 60
	for _, s := range sent {
		d.minutes = append(d.minutes, Minute{Minute: now - int64(s[0]), N: s[1]})
		d.total += s[1]
	}
	return d
}

// wait is Wait's first answer.
func wait(d *Counter, center string, stopped func() bool) bool {
	ok, _ := d.Wait(center, stopped)
	return ok
}

// The count is of the last 24 hours: older minutes drop out.
func TestDailyRolling(t *testing.T) {
	d := counter(0, [2]int{24 * 60, 50}, [2]int{24*60 - 1, 7}, [2]int{3, 5})
	if s := d.Status(); s.Sent != 12 {
		t.Fatalf("sent %d, want 12 (50 are over 24 hours old)", s.Sent)
	}
	d.Add()
	if s := d.Status(); s.Sent != 13 || d.pending[time.Now().Unix()/60] != 1 {
		t.Fatalf("after one more: sent %d, pending %v", s.Sent, d.pending)
	}
	if d.Left() != -1 {
		t.Fatal("no limit should leave -1")
	}
}

// At the limit, the next one can go when enough of the oldest drop out.
func TestDailyFreesAt(t *testing.T) {
	d := counter(10, [2]int{600, 4}, [2]int{300, 6})
	s := d.Status()
	if s.Left != 0 || s.FreesAt.IsZero() {
		t.Fatalf("left %d, frees at %v; want reached", s.Left, s.FreesAt)
	}
	// The 4 sent 600 minutes ago drop out in 840 minutes.
	if got := time.Until(s.FreesAt).Round(time.Minute); got < 839*time.Minute || got > 841*time.Minute {
		t.Fatalf("frees in %v, want about 840m", got)
	}
	d.SetLimit(15, 0)
	if d.Left() != 5 {
		t.Fatalf("left %d at limit 15, want 5", d.Left())
	}
}

// A campaign message waits while the limit is reached, and stops waiting when
// its campaign stops.
func TestDailyWaitStops(t *testing.T) {
	d := counter(1, [2]int{1, 1})
	stop := make(chan struct{})
	done := make(chan bool)
	go func() {
		done <- wait(d, "", func() bool {
			select {
			case <-stop:
				return true
			default:
				return false
			}
		})
	}()
	select {
	case <-done:
		t.Fatal("went over the limit")
	case <-time.After(300 * time.Millisecond):
	}
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("wait returned true for a stopped campaign")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still waiting after the campaign stopped")
	}
	d.SetLimit(2, 0)
	if !wait(d, "", func() bool { return false }) {
		t.Fatal("under the limit, a message should go")
	}
}

// Messages waiting together for the last of the limit: one goes, counted as
// it's let go, and the others wait (tofix 25: each used to see room and go).
func TestDailyWaitTakesOne(t *testing.T) {
	d := counter(10, [2]int{1, 9})
	stop := make(chan struct{})
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	went := make(chan bool, 50)
	for range 50 {
		go func() { went <- wait(d, "", stopped) }()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	n := 0
	for range 50 {
		if <-went {
			n++
		}
	}
	if s := d.Status(); n != 1 || s.Sent != 10 {
		t.Fatalf("%d of 50 went, %d sent; want 1 and 10", n, s.Sent)
	}
}

// Campaigns stop short of the limit by the reserve, which other e-mails may
// still use.
func TestDailyReserve(t *testing.T) {
	d := counter(0, [2]int{60, 90})
	d.SetLimit(100, 5)
	s := d.Status()
	if s.CampaignLimit != 95 || s.Left != 5 {
		t.Fatalf("campaign limit %d, left %d; want 95 and 5", s.CampaignLimit, s.Left)
	}
	for range 5 {
		d.Add()
	}
	if s := d.Status(); s.Left != 0 || s.FreesAt.IsZero() {
		t.Fatalf("at 95 of 100 with a 5%% reserve: left %d, frees at %v; want reached", s.Left, s.FreesAt)
	}
	// Opt-ins and resets still go (they're only counted), past the limit too.
	for range 10 {
		d.Add()
	}
	if s := d.Status(); s.Sent != 105 || s.Left != 0 {
		t.Fatalf("sent %d, left %d", s.Sent, s.Left)
	}
}

// Automations claim their share before sending: two claims can't take the
// same room, sends turn claims into counts, and what isn't sent is given
// back.
func TestDailyClaim(t *testing.T) {
	if got, id := counter(0).Claim(500); got != 500 || id != 0 {
		t.Fatalf("no limit: got %d, claim %d; want 500, none", got, id)
	}

	// A limit of 1,000 with a 10% reserve: 900 for campaigns and automations,
	// 600 of them sent.
	d := counter(1000, [2]int{5, 600})
	d.reserve = 10
	got, first := d.Claim(200)
	if got != 200 || first == 0 {
		t.Fatalf("first claim: got %d, claim %d; want 200, one", got, first)
	}
	got, second := d.Claim(200)
	if got != 100 || second == first {
		t.Fatalf("second claim: got %d (want the 100 left), claim %d (the first's %d)", got, second, first)
	}
	if got, id := d.Claim(50); got != 0 || id != 0 {
		t.Fatalf("third claim: got %d, claim %d; want 0, none", got, id)
	}
	if s := d.Status(); s.Left != 0 || s.Claimed != 300 {
		t.Fatalf("after claiming: left %d, claimed %d; want 0, 300", s.Left, s.Claimed)
	}

	// 150 of the first 200 sent, the other 50 given back.
	for range 150 {
		d.AddClaimed(first)
	}
	d.Release(first, 50)
	if s := d.Status(); s.Sent != 750 || s.Claimed != 100 || s.Left != 50 {
		t.Fatalf("after sending: sent %d, claimed %d, left %d; want 750, 100, 50", s.Sent, s.Claimed, s.Left)
	}
	// A late send of a claim given back counts, and takes nothing off another's.
	d.AddClaimed(first)
	d.Release(first, 10)
	if s := d.Status(); s.Sent != 751 || s.Claimed != 100 {
		t.Fatalf("after a late send: sent %d, claimed %d; want 751, 100", s.Sent, s.Claimed)
	}

	// A claim left untouched for claimTimeout is dropped; another isn't.
	_, third := counter(0).Claim(1) // unclaimed: no limit
	d.claims[second].at = time.Now().Add(-claimTimeout - time.Minute)
	got, fourth := d.Claim(20)
	if s := d.Status(); got != 20 || s.Claimed != 20 || s.Left != 129 {
		t.Fatalf("after the timeout: got %d, claimed %d, left %d; want 20, 20, 129", got, s.Claimed, s.Left)
	}
	d.claims[fourth].at = time.Now().Add(-time.Hour) // an hour isn't enough
	if s := d.Status(); s.Claimed != 20 {
		t.Fatalf("a claim dropped after an hour: claimed %d, want 20", s.Claimed)
	}
	// Its late sends count, and take nothing off the newer claim.
	d.AddClaimed(second)
	d.AddClaimed(third)
	if s := d.Status(); s.Claimed != 20 || s.Sent != 753 {
		t.Fatalf("sends of a dropped claim: claimed %d, sent %d; want 20, 753", s.Claimed, s.Sent)
	}

	// Sends that weren't claimed don't touch the claims.
	d.Add()
	if s := d.Status(); s.Claimed != 20 || s.Sent != 754 {
		t.Fatalf("an unclaimed send: claimed %d, sent %d; want 20, 754", s.Claimed, s.Sent)
	}
}

// Each center has its own count and cap; the hub's isn't touched.
func TestCentersSeparate(t *testing.T) {
	cs := NewCounters(log.New(os.Stderr, "", 0))
	a, b := cs.Get("a"), cs.Get("b")
	if cs.Get("a") != a {
		t.Fatal("Get made a second counter for the same center")
	}
	a.SetLimit(2, 0)
	a.Add()
	a.Add()
	b.Add()
	if a.Left() != 0 || b.Left() != -1 {
		t.Fatalf("a left %d (want 0, at its cap), b left %d (want -1, no cap)", a.Left(), b.Left())
	}
	if got, id := a.Claim(5); got != 0 || id != 0 {
		t.Fatalf("a claimed %d at its cap (claim %d)", got, id)
	}
	b.SetLimit(10, 0)
	if got, _ := b.Claim(20); got != 9 {
		t.Fatalf("b claimed %d, want the 9 left", got)
	}
	cs.Save() // no database yet: a no-op, counts kept
	if a.pending[time.Now().Unix()/60] != 2 {
		t.Fatalf("a's pending counts lost: %v", a.pending)
	}
}

// never is a stopped() for a campaign that isn't stopped.
func never() bool { return false }

// grants starts a waiter of center, and returns a channel that gets its
// center when it's let go.
func grants(d *Counter, center string, out chan<- string) {
	go func() {
		if ok, _ := d.Wait(center, never); ok {
			out <- center
		}
	}()
	time.Sleep(20 * time.Millisecond) // queued, in the order started
}

// Waiting messages go in order, one center's after another's in turn, as
// room frees: no center's queue takes it all (tofix 103).
func TestDailyWaitInTurn(t *testing.T) {
	d := counter(10, [2]int{1, 10})
	out := make(chan string, 10)
	for _, c := range []string{"a", "a", "a", "b", "c"} {
		grants(d, c, out)
	}
	var got []string
	for i := range 5 {
		d.SetLimit(11+i, 0) // room for one more
		select {
		case c := <-out:
			got = append(got, c)
		case <-time.After(time.Second):
			t.Fatalf("no message went after room for %d; got %v", i+1, got)
		}
	}
	if fmt.Sprint(got) != "[a b c a a]" {
		t.Fatalf("went in the order %v, want [a b c a a]", got)
	}
}

// Room freed wakes a waiting message at once, without waiting for a poll.
func TestDailyWaitWakes(t *testing.T) {
	d := counter(10, [2]int{1, 5})
	got, id := d.Claim(5)
	if got != 5 {
		t.Fatalf("claimed %d, want 5", got)
	}
	out := make(chan string, 1)
	grants(d, "a", out)
	select {
	case <-out:
		t.Fatal("a message went with no room")
	default:
	}
	d.Release(id, 5)
	select {
	case <-out:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("room was freed and the message didn't go at once")
	}
}

// A claim refused for lack of room is left room: waiting campaign messages
// don't take the room that frees first (tofix 102).
func TestDailyClaimBeforeWaiters(t *testing.T) {
	d := counter(10, [2]int{1, 10})
	if _, ok := d.ClaimAll(1); ok {
		t.Fatal("a claim at the limit went")
	}
	out := make(chan string, 1)
	grants(d, "a", out)
	d.SetLimit(11, 0)
	select {
	case <-out:
		t.Fatal("a campaign message took the room a refused claim needs")
	case <-time.After(100 * time.Millisecond):
	}
	if _, ok := d.ClaimAll(1); !ok {
		t.Fatal("the claim wasn't given the room that freed")
	}
	d.SetLimit(12, 0)
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("the campaign message didn't go once the claim was served")
	}
}

// Refused claims are left room as one, the largest, however often they're
// tried; one that could never fit is left none.
func TestDailyDemandOnce(t *testing.T) {
	d := counter(10, [2]int{1, 10})
	for range 5 {
		d.ClaimAll(3)
	}
	d.ClaimAll(2)
	d.ClaimAll(holdMax + 1)
	d.mu.Lock()
	held := d.hold(time.Now())
	d.mu.Unlock()
	if held != 3 {
		t.Fatalf("held %d, want 3", held)
	}
}

// Room left for a refused claim is left for a while only.
func TestDailyDemandExpires(t *testing.T) {
	d := counter(10, [2]int{1, 10})
	d.ClaimAll(3)
	d.demands[0].until = time.Now().Add(-time.Second)
	d.SetLimit(11, 0)
	if s := d.Status(); s.Left != 1 {
		t.Fatalf("left %d, want 1", s.Left)
	}
	out := make(chan string, 1)
	grants(d, "a", out)
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("an expired demand still held the room")
	}
}

// A message let go and not sent gives its count back; one that gives up
// waiting leaves its queue, and takes nothing.
func TestDailyGiveBack(t *testing.T) {
	d := counter(10, [2]int{1, 5})
	ok, back := d.Wait("a", never)
	if !ok || d.Status().Sent != 6 {
		t.Fatalf("ok %v, sent %d; want it counted", ok, d.Status().Sent)
	}
	back()
	if s := d.Status(); s.Sent != 5 || len(d.pending) != 0 {
		t.Fatalf("sent %d, pending %v; want the count given back", s.Sent, d.pending)
	}

	d.SetLimit(5, 0)
	var stop atomic.Bool
	done := make(chan bool)
	go func() { ok, _ := d.Wait("a", stop.Load); done <- ok }()
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a stopped wait went")
		}
	case <-time.After(pollEvery + time.Second):
		t.Fatal("still waiting after the campaign stopped")
	}
	d.mu.Lock()
	n := len(d.order) + len(d.queues)
	d.mu.Unlock()
	if n != 0 || d.Status().Sent != 5 {
		t.Fatalf("queue left %d, sent %d; want none, 5", n, d.Status().Sent)
	}
}

// Many waiters go one by one as room frees, each exactly once.
func TestDailyManyWaiters(t *testing.T) {
	d := counter(10, [2]int{1, 10})
	out := make(chan string, 500)
	for i := range 500 {
		go func() {
			if ok, _ := d.Wait(fmt.Sprint(i%7), never); ok {
				out <- ""
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	d.SetLimit(10+300, 0)
	time.Sleep(300 * time.Millisecond)
	if s := d.Status(); len(out) != 300 || s.Sent != 310 {
		t.Fatalf("%d went, %d sent; want 300 and 310", len(out), s.Sent)
	}
}

// An automation's due e-mails that got no room are left room, replaced each
// time it's seen and cleared with 0; ClaimAll's successes don't touch them.
func TestDailyWant(t *testing.T) {
	d := counter(10, [2]int{1, 10})
	out := make(chan string, 1)
	d.Want("c/1", 2)
	d.Want("c/1", 1) // replaces
	grants(d, "a", out)
	d.SetLimit(11, 0)
	select {
	case <-out:
		t.Fatal("a campaign message took the room an automation needs")
	case <-time.After(100 * time.Millisecond):
	}
	if got, _ := d.Claim(5); got != 1 {
		t.Fatalf("the automation claimed %d, want the 1 left", got)
	}
	if len(d.demands) != 1 {
		t.Fatalf("demands %v, want the automation's kept until it says otherwise", d.demands)
	}
	d.Want("c/1", 0) // nothing due any more: the campaign message goes
	d.Release(1, 1)
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("the campaign message didn't go once nothing was wanted")
	}
}
