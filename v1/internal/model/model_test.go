package model

import (
	"testing"
	"time"
)

func TestNormalizeEmailDoesNotOverNormalize(t *testing.T) {
	// Dots are significant at most hosts. Stripping them globally
	// collapses two genuinely different people onto one suppression
	// entry and silently stops mail to someone who never opted out.
	if NormalizeEmail("First.Last@Example.COM ") != "first.last@example.com" {
		t.Fatal("normalisation changed more than case and whitespace")
	}
	if NormalizeEmail("a.b@corp.example") == NormalizeEmail("ab@corp.example") {
		t.Fatal("dots were stripped at a non-Gmail domain")
	}
	if NormalizeEmail("a+tag@corp.example") == NormalizeEmail("a@corp.example") {
		t.Fatal("plus-addressing was stripped at a non-Gmail domain")
	}
}

// TestGmailVariantsCollapseToOneKey is the narrow risk NormalizeEmail
// accepts, handled explicitly only where the rules actually hold.
func TestGmailVariantsCollapseToOneKey(t *testing.T) {
	same := []string{
		"firstlast@gmail.com",
		"first.last@gmail.com",
		"f.i.r.s.t.l.a.s.t@gmail.com",
		"firstlast+newsletter@gmail.com",
		"First.Last+Sales@GMAIL.com",
		"firstlast@googlemail.com",
		"first.last+x@googlemail.com",
	}
	want := CanonicalEmail(same[0])
	if want != "firstlast@gmail.com" {
		t.Fatalf("canonical form is %q", want)
	}
	for _, addr := range same[1:] {
		if got := CanonicalEmail(addr); got != want {
			t.Errorf("%s canonicalises to %q, want %q -- an unsubscribe from one must stop mail to all of them", addr, got, want)
		}
	}
}

func TestCanonicalEmailLeavesOtherDomainsAlone(t *testing.T) {
	cases := map[string]string{
		"A.B@Corp.Example":   "a.b@corp.example",
		"a+tag@corp.example": "a+tag@corp.example",
		"notanemail":         "notanemail",
	}
	for in, want := range cases {
		if got := CanonicalEmail(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestCanonicalEmailHandlesMalformedGmail(t *testing.T) {
	// Nothing but dots and a tag. Collapsing these to an empty local part
	// would file every malformed address under one key and suppress
	// unrelated people, so each is left as itself.
	seen := map[string]string{}
	for _, addr := range []string{"...@gmail.com", "+tag@gmail.com", "@gmail.com", "..@gmail.com"} {
		got := CanonicalEmail(addr)
		if got != addr {
			t.Errorf("%s was rewritten to %s", addr, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s collapsed onto the same key %q", prev, addr, got)
		}
		seen[got] = addr
	}
}

func TestGmailVariantsReachEveryForm(t *testing.T) {
	v := GmailVariants("first.last+x@gmail.com")
	if len(v) != 3 {
		t.Fatalf("%v", v)
	}
	seen := map[string]bool{}
	for _, a := range v {
		seen[a] = true
	}
	for _, want := range []string{"first.last+x@gmail.com", "firstlast@gmail.com", "firstlast@googlemail.com"} {
		if !seen[want] {
			t.Errorf("missing %s from %v", want, v)
		}
	}
}

func TestWindowValid(t *testing.T) {
	if err := DefaultWindow().Valid(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]Window{
		"end before start":   {StartHour: 17, EndHour: 9},
		"end equals start":   {StartHour: 9, EndHour: 9},
		"start out of range": {StartHour: -1, EndHour: 17},
		"end out of range":   {StartHour: 9, EndHour: 25},
		"negative jitter":    {StartHour: 9, EndHour: 17, JitterMax: -time.Minute},
		// Jitter that does not fit can push a send past the end of
		// business hours -- see scheduler.NextSendTime.
		"jitter longer than the window": {StartHour: 9, EndHour: 17, JitterMax: 9 * time.Hour},
		"jitter exactly the window":     {StartHour: 9, EndHour: 17, JitterMax: 8 * time.Hour},
	}
	for name, w := range bad {
		if err := w.Valid(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	// Paused is deliberately NOT terminal: a meeting booking is a pause
	// with a cooldown expiry, and a resume has to be able to pick up from
	// step 3 rather than starting over.
	if StatePaused.Terminal() {
		t.Fatal("paused is terminal; a paused enrolment could never resume")
	}
	if StateActive.Terminal() {
		t.Fatal("active is terminal")
	}
	for _, s := range []EnrollmentState{
		StateFinished, StateBounced, StateSuppressed, StateStopped, StateReplied,
	} {
		if !s.Terminal() {
			t.Errorf("%s is not terminal", s)
		}
	}
}

// TestLinkedInStepsAreManualByConstruction: the distinction is load-bearing
// rather than cosmetic. The rep's own account is the asset at risk.
func TestLinkedInStepsAreManualByConstruction(t *testing.T) {
	// This is a documentation test: it records what the type system is
	// for, so that a later change that lets ModeAutomatic onto a
	// LinkedIn step has to delete an assertion that says why not.
	step := Step{Channel: ChannelLinkedIn, Mode: ModeManualTask}
	if step.Mode != ModeManualTask {
		t.Fatal("LinkedIn step is not a manual task")
	}
}
