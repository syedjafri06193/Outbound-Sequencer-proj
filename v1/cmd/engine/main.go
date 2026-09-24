// Command engine runs a capacity plan through the scheduler and the gates,
// and prints what would actually be sent.
//
// It is a dry run by design. The value of this binary is that it answers
// the question an operator has before they start -- "does my plan fit?" --
// with the same arithmetic the running engine uses, rather than with an
// optimistic estimate from a different code path.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/deliverability"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/model"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/ratelimit"
	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/sequence"
)

func main() {
	var (
		prospects = flag.Int("prospects", 2000, "prospects to enrol")
		steps     = flag.Int("steps", 4, "email steps per sequence")
		days      = flag.Int("days", 21, "days the sequence spans")
		mailboxes = flag.Int("mailboxes", 1, "warmed mailboxes available")
		perBox    = flag.Int("per-mailbox", 40, "cold sends per mailbox per day")
		domains   = flag.Int("domains", 1, "sending domains")
		warmupDay = flag.Int("warmup-day", 28, "how far into warmup each mailbox is")
		tzName    = flag.String("tz", "America/New_York", "recipient timezone")
	)
	flag.Parse()

	if err := run(*prospects, *steps, *days, *mailboxes, *perBox, *domains, *warmupDay, *tzName); err != nil {
		fmt.Fprintln(os.Stderr, "engine:", err)
		os.Exit(1)
	}
}

func run(prospects, steps, days, mailboxes, perBox, domains, warmupDay int, tzName string) error {
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		return err
	}

	fmt.Println("CAPACITY")
	fmt.Println("--------")

	plan := deliverability.PlanCapacity(prospects, steps, days, perBox, ratelimit.DefaultLimits().DomainDaily)
	fmt.Printf("%d prospects x %d steps over %d days = %d sends, %d a day\n",
		prospects, steps, days, plan.TotalSends, plan.SendsPerDay)
	fmt.Printf("at %d sends per mailbox per day, that needs %d mailboxes\n",
		perBox, plan.MailboxesNeeded)
	if plan.Note != "" {
		fmt.Println(plan.Note)
	}

	// Warmup is enforced, not advised.
	started := time.Now().AddDate(0, 0, -warmupDay)
	mb := &model.Mailbox{
		WarmupStartedAt: &started,
		WarmupDay:       warmupDay,
		HealthState:     model.HealthGood,
		DailyCap:        perBox,
	}
	cap := deliverability.DefaultWarmup().Cap(mb, perBox)
	fmt.Printf("\neach mailbox is on warmup day %d, so its cap today is %d (target %d)\n", warmupDay, cap, perBox)

	ids := make([]string, mailboxes)
	caps := make(map[string]int, mailboxes)
	for i := range ids {
		ids[i] = fmt.Sprintf("mb%02d", i+1)
		caps[ids[i]] = cap
	}

	limits := ratelimit.DefaultLimits()
	limits.MailboxDaily = perBox
	if err := limits.Validate(); err != nil {
		return err
	}
	fit := ratelimit.PlanFits(plan.SendsPerDay, ids, limits, caps)
	fmt.Println(fit.Message)

	perDomain := plan.SendsPerDay / max(domains, 1)
	fmt.Printf("\nDOMAINS\n-------\n%d domain(s) carrying %d sends a day each.\n", domains, perDomain)
	if perDomain >= ratelimit.BulkThreshold {
		fmt.Printf("*** %d/day crosses the %d/day Gmail bulk-sender threshold. Crossing it ONCE\n"+
			"    classifies the domain permanently. Add domains or reduce volume.\n",
			perDomain, ratelimit.BulkThreshold)
	} else if float64(perDomain) >= 0.9*float64(ratelimit.BulkThreshold) {
		fmt.Printf("    %d/day is within 10%% of the %d/day bulk threshold.\n", perDomain, ratelimit.BulkThreshold)
	} else {
		fmt.Printf("    comfortably below the %d/day bulk threshold.\n", ratelimit.BulkThreshold)
	}

	fmt.Println("\nCOMPLAINT BUDGET")
	fmt.Println("----------------")
	budget := deliverability.DefaultBudget()
	inboxed := plan.SendsPerDay * 3 / 4 // a rough inbox placement assumption
	allowed := deliverability.ComplaintsAllowed(inboxed, budget.HaltThreshold)
	fmt.Printf("at roughly %d inboxed a day, the halt threshold of %.3f%% is %d complaint(s).\n",
		inboxed, budget.HaltThreshold*100, allowed)
	fmt.Printf("Google's ceiling of %.1f%% would be %d, but the ceiling is where enforcement\n"+
		"begins, not where risk begins.\n",
		deliverability.GoogleCeilingRate*100,
		deliverability.ComplaintsAllowed(inboxed, deliverability.GoogleCeilingRate))
	if allowed == 0 {
		fmt.Println("*** At this volume a SINGLE complaint is already over the halt threshold.")
	}

	fmt.Println("\nSCHEDULE")
	fmt.Println("--------")
	cal := sequence.NewStaticCalendar()
	s := sequence.NewScheduler(cal, 1)
	w := model.DefaultWindow()
	if err := w.Valid(); err != nil {
		return err
	}
	t := time.Now().In(tz)
	for i := 0; i < steps; i++ {
		next, ok := s.NextSendTime(t, tz, "US", w)
		if !ok {
			return fmt.Errorf("no send slot found within %d days", sequence.MaxSearchDays)
		}
		fmt.Printf("  step %d: %s (%s local)\n", i+1, next.Format(time.RFC1123), tzName)
		t = next.Add(time.Duration(days/max(steps, 1)) * 24 * time.Hour)
	}

	fmt.Println("\nBACKPRESSURE")
	fmt.Println("------------")
	capacity := 0
	for _, c := range caps {
		capacity += c
	}
	est := sequence.EstimateSpill(0, capacity, plan.SendsPerDay)
	fmt.Println(" ", est.Recommendation)

	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
