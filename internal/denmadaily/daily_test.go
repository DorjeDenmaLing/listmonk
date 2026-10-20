package denmadaily

// The daily sending limit's count.

import (
	"log"
	"os"
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
		done <- d.Wait(func() bool {
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
	if !d.Wait(func() bool { return false }) {
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
		go func() { went <- d.Wait(stopped) }()
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
