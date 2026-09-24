// Command postmaster-sync reads a Google Postmaster Tools CSV export,
// replays it through the complaint budget circuit breaker, and reports what
// the breaker would have done on each day.
//
// Replaying history is the only honest way to pick thresholds. Setting the
// halt threshold by argument produces a number somebody feels good about;
// replaying last quarter against it produces a number you know the
// consequences of.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/deliverability"
)

func main() {
	var (
		path   = flag.String("csv", "", "Postmaster Tools CSV export (required)")
		domain = flag.String("domain", "unknown", "the sending domain the export is for")
		halt   = flag.Float64("halt", deliverability.DefaultBudget().HaltThreshold, "halt threshold as a fraction")
		warn   = flag.Float64("warn", deliverability.DefaultBudget().WarnThreshold, "warn threshold as a fraction")
		minVol = flag.Int("min-volume", deliverability.DefaultBudget().MinVolume, "minimum inboxed volume before acting")
	)
	flag.Parse()

	if *path == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*path, *domain, *halt, *warn, *minVol); err != nil {
		fmt.Fprintln(os.Stderr, "postmaster-sync:", err)
		os.Exit(1)
	}
}

func run(path, domain string, halt, warn float64, minVol int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	rows, err := deliverability.ParseCSV(f, domain)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no rows in %s", path)
	}

	budget := deliverability.DefaultBudget()
	budget.HaltThreshold = halt
	budget.WarnThreshold = warn
	budget.MinVolume = minVol

	breaker := deliverability.NewBreaker(budget)

	fmt.Printf("REPLAY: %s, %d days, halt at %.3f%%, warn at %.3f%%\n\n",
		domain, len(rows), halt*100, warn*100)
	fmt.Printf("%-12s %9s %11s %9s  %s\n", "DATE", "INBOXED", "COMPLAINTS", "RATE", "ACTION")

	firstHalt := -1
	for i, row := range rows {
		m := row.ToMetrics()
		d := breaker.Observe(m)

		rate, ok := m.ComplaintRate()
		rateStr := "     n/a"
		if ok {
			rateStr = fmt.Sprintf("%7.3f%%", rate*100)
		}
		fmt.Printf("%-12s %9d %11d %9s  %s\n",
			row.Date.Format("2006-01-02"), m.Inboxed, m.Complaints, rateStr, d.Action)

		if d.Action == deliverability.HaltDomain && firstHalt < 0 {
			firstHalt = i
			fmt.Printf("%-12s %9s %11s %9s  %s\n", "", "", "", "", d.Reason)
		}

		if h := row.AuthHealth(); !h.OK {
			fmt.Printf("%-12s %9s %11s %9s  auth: %s\n", "", "", "", "", h.Problem)
		}
		if row.DomainRep.Degraded() {
			fmt.Printf("%-12s %9s %11s %9s  domain reputation is %s\n",
				"", "", "", "", row.DomainRep)
		}
	}

	fmt.Println()
	if firstHalt >= 0 {
		fmt.Printf("HALTED on %s, day %d of %d.\n", rows[firstHalt].Date.Format("2006-01-02"), firstHalt+1, len(rows))
		fmt.Println("It stays halted. An automatic resume means the same behaviour resumes")
		fmt.Println("and the spiral continues; a human has to look at what happened.")
		for _, h := range breaker.HaltedDomains() {
			fmt.Printf("  %s: %s\n", h.DomainID, h.Reason)
		}
	} else {
		fmt.Println("No halt over this period.")
	}

	trend := deliverability.AnalyseTrend(rows)
	fmt.Println("\nTREND")
	fmt.Println(" ", trend.Message)
	if trend.DenominatorCollapse {
		fmt.Println("\n  This is the shape a breaker watching raw complaint COUNTS misses")
		fmt.Println("  entirely. The rate is complaints over INBOXED, and inboxed is the")
		fmt.Println("  number that moved.")
	}

	return nil
}
