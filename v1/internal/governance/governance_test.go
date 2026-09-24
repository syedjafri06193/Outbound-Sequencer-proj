package governance

import (
	"sync"
	"testing"
	"time"
)

func now() time.Time { return time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) }

func ledger() *Ledger {
	l := NewLedger(DefaultPolicy())
	l.SetClock(now)
	return l
}

// TestFiveRepsEmailedMe is §6.3 in one test: four reps working the same
// target account, each within their own numbers, producing eight cold
// emails to one company in a week.
func TestFiveRepsEmailedMe(t *testing.T) {
	l := ledger()

	delivered := 0
	for rep := 0; rep < 4; rep++ {
		for contact := 0; contact < 2; contact++ {
			if !l.CheckSend("acme").Allowed {
				continue
			}
			l.RecordTouch(Touch{
				AccountID: "acme",
				ContactID: "c" + string(rune('0'+contact)),
				RepID:     "rep" + string(rune('0'+rep)),
				Channel:   "email",
				At:        now(),
			})
			delivered++
		}
	}

	if delivered != DefaultPolicy().MaxTouchesPerWeek {
		t.Fatalf("Acme received %d cold emails; the limit is %d", delivered, DefaultPolicy().MaxTouchesPerWeek)
	}
	t.Logf("four reps with two contacts each would have sent 8; account governance allowed %d", delivered)
}

func TestCheckSendIsRecheckedNotTrustedFromEnrolment(t *testing.T) {
	l := ledger()
	// Enrolment passes when the account is quiet.
	if !l.CheckEnroll("acme", "c1").Allowed {
		t.Fatal("enrolment refused on a quiet account")
	}
	l.Enroll("acme", "c1")

	// Three other reps touch the account in the meantime.
	for i := 0; i < 3; i++ {
		l.RecordTouch(Touch{AccountID: "acme", ContactID: "other", RepID: "rep" + string(rune('0'+i)), At: now()})
	}

	if l.CheckSend("acme").Allowed {
		t.Fatal("the send-time check trusted the enrolment-time decision; the aggregate is the whole failure mode")
	}
}

func TestInFlightLimit(t *testing.T) {
	l := ledger()
	l.Enroll("acme", "c1")
	l.Enroll("acme", "c2")
	if l.CheckEnroll("acme", "c3").Allowed {
		t.Fatal("a third simultaneous enrolment was allowed against a limit of 2")
	}
	// Re-enrolling someone already in flight must not count twice.
	if !l.CheckEnroll("acme", "c1").Allowed {
		t.Fatal("re-checking an already-enrolled contact was refused")
	}
	l.Unenroll("acme", "c1")
	if !l.CheckEnroll("acme", "c3").Allowed {
		t.Fatal("a slot did not free up after unenrolment")
	}
}

func TestCooldownBlocksSends(t *testing.T) {
	l := ledger()
	l.StartCooldown("acme", DefaultPolicy().CooldownAfterBooking, "meeting booked")

	d := l.CheckSend("acme")
	if d.Allowed {
		t.Fatal("cooldown did not block")
	}
	if d.Reason == "" {
		t.Fatal("blocked without saying why")
	}

	l.SetClock(func() time.Time { return now().AddDate(0, 0, 31) })
	if !l.CheckSend("acme").Allowed {
		t.Fatal("cooldown did not expire")
	}
}

// TestCooldownExtendsButNeverShortens: a booking cooldown must not be
// truncated by a later, shorter reply cooldown.
func TestCooldownExtendsButNeverShortens(t *testing.T) {
	l := ledger()
	l.StartCooldown("acme", 30*24*time.Hour, "meeting booked")
	l.StartCooldown("acme", 24*time.Hour, "reply")

	l.SetClock(func() time.Time { return now().AddDate(0, 0, 2) })
	if l.CheckSend("acme").Allowed {
		t.Fatal("a one-day cooldown shortened a thirty-day one")
	}
}

func TestTouchWindowRollsForward(t *testing.T) {
	l := ledger()
	for i := 0; i < 3; i++ {
		l.RecordTouch(Touch{AccountID: "acme", At: now()})
	}
	if l.CheckSend("acme").Allowed {
		t.Fatal("fourth touch inside the week was allowed")
	}
	l.SetClock(func() time.Time { return now().AddDate(0, 0, 8) })
	if !l.CheckSend("acme").Allowed {
		t.Fatal("the week did not roll")
	}
	if n := l.TouchesInWeek("acme"); n != 0 {
		t.Fatalf("%d touches still counted", n)
	}
}

func TestPrune(t *testing.T) {
	l := ledger()
	l.RecordTouch(Touch{AccountID: "acme", At: now().AddDate(0, 0, -60)})
	l.RecordTouch(Touch{AccountID: "acme", At: now()})
	if n := l.Prune(30 * 24 * time.Hour); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if l.TouchesInWeek("acme") != 1 {
		t.Fatal("pruning removed a live touch")
	}
}

// TestBusiestAccountsReportsRepCount is the operator view: an account being
// worked by four reps is the thing someone needs to see.
func TestBusiestAccountsReportsRepCount(t *testing.T) {
	l := ledger()
	for rep := 0; rep < 4; rep++ {
		l.RecordTouch(Touch{AccountID: "acme", RepID: "rep" + string(rune('0'+rep)), At: now()})
	}
	l.RecordTouch(Touch{AccountID: "quiet", RepID: "rep0", At: now()})

	got := l.BusiestAccounts(5)
	if len(got) == 0 || got[0].AccountID != "acme" {
		t.Fatalf("%+v", got)
	}
	if got[0].Reps != 4 {
		t.Fatalf("acme shows %d reps, want 4", got[0].Reps)
	}
}

func TestConcurrentLedger(t *testing.T) {
	l := ledger()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.CheckSend("acme")
			l.RecordTouch(Touch{AccountID: "acme", At: now()})
			l.TouchesInWeek("acme")
		}(i)
	}
	wg.Wait()
	if l.TouchesInWeek("acme") != 200 {
		t.Fatalf("%d touches recorded, want 200", l.TouchesInWeek("acme"))
	}
}
