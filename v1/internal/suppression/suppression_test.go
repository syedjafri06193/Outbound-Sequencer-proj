package suppression

import (
	"sync"
	"testing"
	"time"
)

func now() time.Time { return time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) }

func list() *List {
	l := New()
	l.SetClock(now)
	return l
}

func TestAddAndCheck(t *testing.T) {
	l := list()
	l.Add("Prospect@Example.com", Unsubscribed, "endpoint", "one-click")
	e := l.Check("prospect@example.com")
	if e == nil {
		t.Fatal("not suppressed")
	}
	if e.Reason != Unsubscribed {
		t.Fatalf("reason %s", e.Reason)
	}
}

// TestSuppressionOutlivesTheContact: records are keyed on the address, not
// on a contact id, so a delete-and-reimport does not resurrect anyone.
func TestSuppressionSurvivesReimport(t *testing.T) {
	l := list()
	l.Add("gone@example.com", Unsubscribed, "endpoint", "")
	// The contact record is deleted and the list re-imported. Nothing
	// here knows or cares.
	allowed, blocked := l.FilterImport([]string{"gone@example.com", "new@example.com"})
	if len(allowed) != 1 || allowed[0] != "new@example.com" {
		t.Fatalf("allowed %v", allowed)
	}
	if blocked["gone@example.com"] != Unsubscribed {
		t.Fatalf("blocked %v", blocked)
	}
}

func TestPermanentReasons(t *testing.T) {
	permanent := []Reason{Unsubscribed, SpamComplaint, HardBounce, HostileReply, DoNotContactCRM}
	for _, r := range permanent {
		if !r.Permanent() {
			t.Errorf("%s should be permanent", r)
		}
	}
	// A cooldown that never lapses quietly removes a prospect from the
	// database, which is the mirror-image bug.
	for _, r := range []Reason{RecentlyContacted, OpenOpportunity, ExistingCustomer} {
		if r.Permanent() {
			t.Errorf("%s should lapse", r)
		}
	}
}

// TestPermanentReasonsCannotBeGivenAnExpiry: an unsubscribe that lapses
// after ninety days is a CAN-SPAM violation with a timer on it.
func TestPermanentReasonsCannotBeGivenAnExpiry(t *testing.T) {
	l := list()
	for _, r := range []Reason{Unsubscribed, SpamComplaint, HardBounce, HostileReply} {
		_, err := l.AddWithExpiry("a@example.com", r, "test", "", now().AddDate(0, 3, 0))
		if err == nil {
			t.Errorf("%s accepted an expiry", r)
		}
	}
}

func TestTemporarySuppressionLapses(t *testing.T) {
	l := list()
	if _, err := l.AddWithExpiry("a@example.com", RecentlyContacted, "engine", "", now().AddDate(0, 0, 14)); err != nil {
		t.Fatal(err)
	}
	if !l.Suppressed("a@example.com") {
		t.Fatal("cooldown not in effect")
	}
	l.SetClock(func() time.Time { return now().AddDate(0, 0, 15) })
	if l.Suppressed("a@example.com") {
		t.Fatal("cooldown did not lapse")
	}
}

// TestPermanentIsReportedOverTemporary: "unsubscribed" and "contacted last
// week" call for entirely different operator responses.
func TestPermanentIsReportedOverTemporary(t *testing.T) {
	l := list()
	_, _ = l.AddWithExpiry("a@example.com", RecentlyContacted, "engine", "", now().AddDate(0, 0, 14))
	l.Add("a@example.com", Unsubscribed, "endpoint", "")
	// And a second cooldown added afterwards must not hide it either.
	_, _ = l.AddWithExpiry("a@example.com", RecentlyContacted, "engine", "", now().AddDate(0, 0, 21))

	e := l.Check("a@example.com")
	if e.Reason != Unsubscribed {
		t.Fatalf("reported %s; an operator would conclude they can email again in three weeks", e.Reason)
	}
}

func TestHistoryIsNeverOverwritten(t *testing.T) {
	l := list()
	l.Add("a@example.com", RecentlyContacted, "engine", "step 1")
	l.Add("a@example.com", Unsubscribed, "endpoint", "one-click")
	l.Add("a@example.com", SpamComplaint, "postmaster", "")

	h := l.History("a@example.com")
	if len(h) != 3 {
		t.Fatalf("%d records; the history is the audit trail that answers a regulator", len(h))
	}
}

// TestGmailVariantsShareOneEntry: an unsubscribe must not be circumventable
// by a differently-formatted import of the same list.
func TestGmailVariantsShareOneEntry(t *testing.T) {
	l := list()
	l.Add("first.last+sales@gmail.com", Unsubscribed, "endpoint", "")
	for _, variant := range []string{
		"firstlast@gmail.com",
		"f.i.r.s.t.last@gmail.com",
		"firstlast+other@googlemail.com",
		"FirstLast@Gmail.com",
	} {
		if !l.Suppressed(variant) {
			t.Errorf("%s is still reachable", variant)
		}
	}
	if l.Count() != 1 {
		t.Fatalf("%d distinct entries for one inbox", l.Count())
	}
}

func TestNonGmailDotsAreNotCollapsed(t *testing.T) {
	l := list()
	l.Add("first.last@corp.example", Unsubscribed, "endpoint", "")
	if l.Suppressed("firstlast@corp.example") {
		t.Fatal("suppressed a genuinely different person at a domain where dots are significant")
	}
}

func TestStats(t *testing.T) {
	l := list()
	l.Add("a@example.com", Unsubscribed, "endpoint", "")
	l.Add("b@example.com", HardBounce, "provider", "")
	l.Add("c@example.com", Unsubscribed, "endpoint", "")
	_, _ = l.AddWithExpiry("d@example.com", RecentlyContacted, "engine", "", now().AddDate(0, 0, -1))

	s := l.Stats()
	if s.Total != 3 {
		t.Fatalf("total %d; the lapsed cooldown should not count", s.Total)
	}
	if s.ByReason[Unsubscribed] != 2 || s.ByReason[HardBounce] != 1 {
		t.Fatalf("%+v", s.ByReason)
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	l := list()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			l.Add("a@example.com", Unsubscribed, "endpoint", "")
		}(i)
		go func() {
			defer wg.Done()
			l.Check("a@example.com")
			l.Stats()
		}()
	}
	wg.Wait()
	if !l.Suppressed("a@example.com") {
		t.Fatal("lost the suppression")
	}
}
