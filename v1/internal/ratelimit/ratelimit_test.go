package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func at(h, m int) time.Time {
	return time.Date(2026, 3, 2, h, m, 0, 0, time.UTC)
}

func noInterval() Limits {
	l := DefaultLimits()
	l.MinInterval = 0
	return l
}

func TestDefaultLimitsAreValid(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesImplausibleAndUnsafeConfigurations(t *testing.T) {
	cases := map[string]Limits{
		"mailbox cap above behavioural plausibility": {MailboxDaily: 250, DomainDaily: 4000, AccountWeekly: 3},
		"domain cap at the bulk threshold":           {MailboxDaily: 40, DomainDaily: BulkThreshold, AccountWeekly: 3},
		"domain cap past the bulk threshold":         {MailboxDaily: 40, DomainDaily: 6000, AccountWeekly: 3},
		"no mailbox cap":                             {MailboxDaily: 0, DomainDaily: 4000, AccountWeekly: 3},
		"no account cap":                             {MailboxDaily: 40, DomainDaily: 4000, AccountWeekly: 0},
	}
	for name, l := range cases {
		if err := l.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMailboxDailyCap(t *testing.T) {
	l := New(noInterval())
	now := at(9, 0)
	for i := 0; i < 40; i++ {
		if ok, r := l.Allow("mb1", "d1", "", now); !ok {
			t.Fatalf("send %d refused: %s", i, r.Message)
		}
	}
	ok, r := l.Allow("mb1", "d1", "", now)
	if ok {
		t.Fatal("41st send allowed against a cap of 40")
	}
	if r.Level != LevelMailbox {
		t.Fatalf("refused at %s, want mailbox", r.Level)
	}
}

func TestWarmupCapOverridesTheGlobalCap(t *testing.T) {
	l := New(noInterval())
	l.SetMailboxCap("mb1", 5)
	now := at(9, 0)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("mb1", "d1", "", now); !ok {
			t.Fatalf("send %d refused under a warmup cap of 5", i)
		}
	}
	if ok, _ := l.Allow("mb1", "d1", "", now); ok {
		t.Fatal("warmup cap of 5 did not hold")
	}
}

// TestZeroWarmupCapIsHonoured is the bug where a halted mailbox gets
// promoted back to the global default because zero reads as "unset".
func TestZeroWarmupCapIsHonoured(t *testing.T) {
	l := New(noInterval())
	l.SetMailboxCap("halted", 0)
	if ok, r := l.Allow("halted", "d1", "", at(9, 0)); ok {
		t.Fatalf("a mailbox with a cap of zero sent anyway (%s)", r.Message)
	}
}

func TestDomainCapHoldsAcrossMailboxes(t *testing.T) {
	// Ten warmed mailboxes on one domain is exactly how a domain reaches
	// the bulk threshold while every mailbox is within its own cap.
	lim := noInterval()
	lim.DomainDaily = 100
	lim.MailboxDaily = 40
	l := New(lim)
	now := at(9, 0)

	sent := 0
	for mb := 0; mb < 10; mb++ {
		id := "mb" + string(rune('0'+mb))
		for i := 0; i < 40; i++ {
			if ok, r := l.Allow(id, "d1", "", now); !ok {
				if r.Level != LevelDomain {
					t.Fatalf("refused at %s, want domain", r.Level)
				}
				goto done
			}
			sent++
		}
	}
done:
	if sent != 100 {
		t.Fatalf("domain let through %d, want 100", sent)
	}
}

// TestAccountCapCatchesTheFiveRepsProblem is the §6.3 scenario: four reps,
// two contacts each, every mailbox inside its own cap.
func TestAccountCapCatchesTheFiveRepsProblem(t *testing.T) {
	l := New(noInterval())
	now := at(9, 0)

	delivered := 0
	for rep := 0; rep < 4; rep++ {
		mailbox := "rep" + string(rune('0'+rep))
		for contact := 0; contact < 2; contact++ {
			ok, r := l.Allow(mailbox, "d1", "acme", now)
			if ok {
				delivered++
				continue
			}
			if r.Level != LevelAccount {
				t.Fatalf("refused at %s, want account", r.Level)
			}
		}
	}
	if delivered != 3 {
		t.Fatalf("Acme received %d cold emails in a week; the cap is 3", delivered)
	}
	t.Logf("four reps with two contacts each would have sent 8 emails to one company; the account cap allowed %d", delivered)
}

func TestAccountWindowRollsForward(t *testing.T) {
	l := New(noInterval())
	start := at(9, 0)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("mb1", "d1", "acme", start); !ok {
			t.Fatalf("touch %d refused", i)
		}
	}
	if ok, _ := l.Allow("mb1", "d1", "acme", start.Add(time.Hour)); ok {
		t.Fatal("fourth touch allowed inside the week")
	}
	// Eight days later the window has rolled.
	if ok, r := l.Allow("mb1", "d1", "acme", start.AddDate(0, 0, 8)); !ok {
		t.Fatalf("refused after the week rolled: %s", r.Message)
	}
}

func TestMinIntervalIsEnforced(t *testing.T) {
	l := New(DefaultLimits())
	now := at(9, 0)
	if ok, _ := l.Allow("mb1", "d1", "", now); !ok {
		t.Fatal("first send refused")
	}
	ok, r := l.Allow("mb1", "d1", "", now.Add(10*time.Second))
	if ok {
		t.Fatal("two sends ten seconds apart; forty emails in ninety seconds is not human")
	}
	if r.Level != LevelInterval {
		t.Fatalf("refused at %s", r.Level)
	}
	if r.RetryAfter <= 0 || r.RetryAfter > 90*time.Second {
		t.Fatalf("retry after %v", r.RetryAfter)
	}
	if ok, _ := l.Allow("mb1", "d1", "", now.Add(91*time.Second)); !ok {
		t.Fatal("refused past the interval")
	}
}

func TestIntervalIsPerMailboxNotGlobal(t *testing.T) {
	l := New(DefaultLimits())
	now := at(9, 0)
	l.Allow("mb1", "d1", "", now)
	if ok, r := l.Allow("mb2", "d1", "", now.Add(time.Second)); !ok {
		t.Fatalf("a second mailbox was blocked by the first one's interval: %s", r.Message)
	}
}

func TestDailyCountersResetAcrossDays(t *testing.T) {
	l := New(noInterval())
	day1 := at(9, 0)
	for i := 0; i < 40; i++ {
		l.Allow("mb1", "d1", "", day1)
	}
	if ok, _ := l.Allow("mb1", "d1", "", day1); ok {
		t.Fatal("cap not reached")
	}
	if ok, _ := l.Allow("mb1", "d1", "", day1.AddDate(0, 0, 1)); !ok {
		t.Fatal("cap did not reset the next day")
	}
}

func TestRefusedSendConsumesNothing(t *testing.T) {
	// A send refused by the account cap must not spend the mailbox's
	// allowance: the operator would lose capacity to a send that never
	// happened.
	l := New(noInterval())
	now := at(9, 0)
	for i := 0; i < 3; i++ {
		l.Allow("mb1", "d1", "acme", now)
	}
	before := l.Remaining("mb1", "d1", "other", now)
	l.Allow("mb1", "d1", "acme", now) // refused at the account level
	after := l.Remaining("mb1", "d1", "other", now)
	if after.Mailbox != before.Mailbox {
		t.Fatalf("a refused send consumed mailbox allowance: %d then %d", before.Mailbox, after.Mailbox)
	}
	if after.Domain != before.Domain {
		t.Fatalf("a refused send consumed domain allowance: %d then %d", before.Domain, after.Domain)
	}
}

// TestCheckAndCommitAreOneOperation is the concurrency property that makes
// the caps real: without it two workers both read 39 against a cap of 40
// and both send.
func TestCheckAndCommitAreOneOperation(t *testing.T) {
	lim := noInterval()
	lim.MailboxDaily = 40
	lim.DomainDaily = 4000
	lim.AccountWeekly = 1000
	l := New(lim)
	now := at(9, 0)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("mb1", "d1", "acme", now); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != 40 {
		t.Fatalf("500 concurrent workers got %d sends past a cap of 40", allowed)
	}
}

func TestRemainingDoesNotConsume(t *testing.T) {
	l := New(noInterval())
	now := at(9, 0)
	a := l.Remaining("mb1", "d1", "acme", now)
	b := l.Remaining("mb1", "d1", "acme", now)
	if a != b {
		t.Fatalf("Remaining consumed allowance: %+v then %+v", a, b)
	}
	if a.Mailbox != 40 || a.AccountWeek != 3 {
		t.Fatalf("%+v", a)
	}
}

func TestPrune(t *testing.T) {
	l := New(noInterval())
	old := at(9, 0)
	l.Allow("mb1", "d1", "acme", old)
	if n := l.Prune(old.AddDate(0, 0, 10)); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if n := l.Prune(old.AddDate(0, 0, 10)); n != 0 {
		t.Fatalf("pruned %d on the second pass", n)
	}
}

func TestBusiestDomainsFlagsTheBulkApproach(t *testing.T) {
	lim := noInterval()
	lim.DomainDaily = 4900
	lim.MailboxDaily = 100
	l := New(lim)
	now := at(9, 0)
	for i := 0; i < 4600; i++ {
		l.Allow("mb"+string(rune('a'+i%50)), "hot", "", now)
	}
	l.Allow("mbz", "cold", "", now)

	got := l.BusiestDomains(now, 2)
	if len(got) != 2 || got[0].DomainID != "hot" {
		t.Fatalf("%+v", got)
	}
	if !got[0].NearBulkThreshold {
		t.Fatalf("4,600/day was not flagged as approaching the %d threshold", BulkThreshold)
	}
	if got[1].NearBulkThreshold {
		t.Fatal("a domain with one send was flagged")
	}
}

// TestPlanFitsProducesTheTenMailboxAnswer is the §2.5 arithmetic: 2,000
// prospects, 4 steps, 21 days, 40/day per mailbox.
func TestPlanFitsProducesTheTenMailboxAnswer(t *testing.T) {
	daily := 2000 * 4 / 21 // 380 sends a day
	l := DefaultLimits()

	one := PlanFits(daily, []string{"mb1"}, l, nil)
	if one.MailboxesNeeded != 9 {
		t.Fatalf("with one mailbox, needs %d more; want 9 (ten in total)", one.MailboxesNeeded)
	}
	t.Logf("2,000 prospects x 4 steps / 21 days = %d sends/day: %s", daily, one.Message)

	ten := make([]string, 10)
	for i := range ten {
		ten[i] = "mb" + string(rune('0'+i))
	}
	full := PlanFits(daily, ten, l, nil)
	if full.MailboxesNeeded != 0 {
		t.Fatalf("ten mailboxes did not fit: %s", full.Message)
	}
}

func TestSpacing(t *testing.T) {
	// 40 sends across an eight-hour window is one every twelve minutes.
	if got := Spacing(40, 8*time.Hour, 90*time.Second); got != 12*time.Minute {
		t.Fatalf("got %v", got)
	}
	// A large allowance is floored at the minimum interval rather than
	// producing a burst.
	if got := Spacing(1000, 8*time.Hour, 90*time.Second); got != 90*time.Second {
		t.Fatalf("got %v", got)
	}
	if got := Spacing(1, 8*time.Hour, 90*time.Second); got != 8*time.Hour {
		t.Fatalf("got %v", got)
	}
}
