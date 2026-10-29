package tmptokens

import (
	"testing"
	"time"
)

// A token allows a few checks, which a peek doesn't count.
func TestDenmaTokenTries(t *testing.T) {
	Set("t1", time.Minute, "v")
	for i := 0; i < 100; i++ {
		if _, err := Peek("t1"); err != nil {
			t.Fatalf("peek %d: %v", i, err)
		}
	}
	for i := 1; i <= maxTries; i++ {
		if v, err := Check("t1"); err != nil || v != "v" {
			t.Fatalf("check %d: %v, %v", i, v, err)
		}
	}
	if _, err := Check("t1"); err == nil {
		t.Fatalf("check %d worked", maxTries+1)
	}
	if _, err := Peek("t1"); err == nil {
		t.Fatal("the token outlived its checks")
	}
	if maxTries != 5 {
		t.Fatalf("maxTries is %d", maxTries)
	}
}
