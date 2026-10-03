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
	d.SetLimit(15)
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
	d.SetLimit(2)
	if !d.Wait(func() bool { return false }) {
		t.Fatal("under the limit, a message should go")
	}
}
