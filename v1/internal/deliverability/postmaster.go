package deliverability

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Postmaster ingestion.
//
// Google Postmaster Tools is the only source that reports the complaint
// rate as Google computes it, on the denominator Google uses. Everything
// else -- feedback loops, local unsubscribe counts, "report spam" clicks
// inferred from anything -- is a different number that happens to have the
// same units. Mixing them produces a figure that looks like a complaint
// rate, is wrong by a factor of several, and is wrong in the reassuring
// direction.
//
// So the parsed rows carry their source, and Metrics.ComplaintRate refuses
// to produce a rate for anything but Postmaster data.

// PostmasterRow is one day of one domain's reputation data.
type PostmasterRow struct {
	Date     time.Time
	DomainID string
	// SpamRate is Google's own figure, already a fraction of inboxed
	// mail. Stored as reported rather than recomputed.
	SpamRate float64
	// InboxedEstimate is Google's delivered-to-inbox volume. Postmaster
	// reports this in buckets rather than exactly, which is why the
	// breaker has a minimum volume: a rate over a bucketed denominator
	// is noise below a few hundred messages.
	InboxedEstimate int
	Complaints      int
	DomainRep       Reputation
	IPRep           Reputation
	// DKIMSuccess, SPFSuccess and DMARCSuccess are authentication pass
	// rates. A drop here precedes a complaint spike, because unauthenticated
	// mail lands in spam and the denominator starts collapsing.
	DKIMSuccess  float64
	SPFSuccess   float64
	DMARCSuccess float64
}

// Reputation is Google's four-level domain and IP reputation.
type Reputation string

const (
	RepHigh    Reputation = "high"
	RepMedium  Reputation = "medium"
	RepLow     Reputation = "low"
	RepBad     Reputation = "bad"
	RepUnknown Reputation = ""
)

// Degraded reports a reputation worth acting on.
//
// "Medium" counts. By the time Google says "low", mail is already going to
// spam and the denominator spiral in §2.3 has started.
func (r Reputation) Degraded() bool {
	switch r {
	case RepMedium, RepLow, RepBad:
		return true
	default:
		return false
	}
}

// ParseReputation is tolerant of the casing variations in exports.
func ParseReputation(s string) Reputation {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return RepHigh
	case "medium":
		return RepMedium
	case "low":
		return RepLow
	case "bad":
		return RepBad
	default:
		return RepUnknown
	}
}

// ParseCSV reads a Postmaster Tools export.
//
// Column order is taken from the header rather than assumed, because the
// export's columns have moved before and a positional parser that silently
// reads the DKIM rate as the spam rate is the worst possible failure here:
// it produces a plausible number.
func ParseCSV(r io.Reader, domainID string) ([]PostmasterRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("postmaster: reading header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}

	required := []string{"date", "spam rate"}
	for _, want := range required {
		if _, ok := col[want]; !ok {
			return nil, fmt.Errorf("postmaster: export has no %q column (columns: %s); refusing to guess",
				want, strings.Join(header, ", "))
		}
	}

	get := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var out []PostmasterRow
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("postmaster: line %d: %w", line, err)
		}

		d, err := time.Parse("2006-01-02", get(rec, "date"))
		if err != nil {
			return nil, fmt.Errorf("postmaster: line %d: bad date %q: %w", line, get(rec, "date"), err)
		}

		row := PostmasterRow{
			Date:            d,
			DomainID:        domainID,
			SpamRate:        parseRate(get(rec, "spam rate")),
			InboxedEstimate: parseInt(get(rec, "inboxed")),
			Complaints:      parseInt(get(rec, "complaints")),
			DomainRep:       ParseReputation(get(rec, "domain reputation")),
			IPRep:           ParseReputation(get(rec, "ip reputation")),
			DKIMSuccess:     parseRate(get(rec, "dkim success rate")),
			SPFSuccess:      parseRate(get(rec, "spf success rate")),
			DMARCSuccess:    parseRate(get(rec, "dmarc success rate")),
		}
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

// parseRate accepts both "0.0012" and "0.12%".
func parseRate(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	pct := strings.HasSuffix(s, "%")
	s = strings.TrimSuffix(s, "%")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	if pct {
		v /= 100
	}
	return v
}

func parseInt(s string) int {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// ToMetrics converts a row into what the breaker evaluates.
//
// Complaints are derived from the reported rate when the export does not
// carry a count, which is the usual case. The derivation is explicit rather
// than hidden, because the count it produces is an estimate over a bucketed
// denominator.
func (r PostmasterRow) ToMetrics() Metrics {
	complaints := r.Complaints
	if complaints == 0 && r.SpamRate > 0 && r.InboxedEstimate > 0 {
		complaints = int(r.SpamRate*float64(r.InboxedEstimate) + 0.5)
	}
	return Metrics{
		DomainID:   r.DomainID,
		Day:        r.Date,
		Source:     SourcePostmaster,
		Inboxed:    r.InboxedEstimate,
		Complaints: complaints,
	}
}

// AuthHealth flags authentication problems.
//
// Worth its own check rather than folding into the complaint rate, because
// it is the leading indicator: unauthenticated mail lands in spam, the
// inboxed denominator shrinks, and the complaint rate climbs on unchanged
// behaviour. Seeing the auth drop first is the difference between fixing a
// DNS record and explaining a halted domain.
type AuthHealth struct {
	OK      bool
	Problem string
}

// AuthMinimum is the pass rate below which authentication is considered
// broken rather than noisy.
const AuthMinimum = 0.95

func (r PostmasterRow) AuthHealth() AuthHealth {
	var problems []string
	if r.DKIMSuccess > 0 && r.DKIMSuccess < AuthMinimum {
		problems = append(problems, fmt.Sprintf("DKIM passing on only %.1f%% of mail", r.DKIMSuccess*100))
	}
	if r.SPFSuccess > 0 && r.SPFSuccess < AuthMinimum {
		problems = append(problems, fmt.Sprintf("SPF passing on only %.1f%% of mail", r.SPFSuccess*100))
	}
	if r.DMARCSuccess > 0 && r.DMARCSuccess < AuthMinimum {
		problems = append(problems, fmt.Sprintf("DMARC passing on only %.1f%% of mail", r.DMARCSuccess*100))
	}
	if len(problems) == 0 {
		return AuthHealth{OK: true}
	}
	return AuthHealth{Problem: strings.Join(problems, "; ") +
		". Unauthenticated mail goes to spam, which shrinks the inboxed denominator and raises the complaint rate on unchanged behaviour."}
}

// Trend summarises a run of days.
type Trend struct {
	Days int
	// InboxedChange is the ratio of the last day's inboxed volume to the
	// first. Below 1 the denominator is collapsing, which is §2.3.
	InboxedChange float64
	RateChange    float64
	// DenominatorCollapse is set when inboxed volume fell materially
	// while the complaint COUNT did not rise. That shape is the spiral,
	// and a breaker watching raw counts misses it entirely.
	DenominatorCollapse bool
	Message             string
}

// CollapseThreshold is the drop in inboxed volume that counts as a
// collapse rather than ordinary variation.
const CollapseThreshold = 0.8

// AnalyseTrend looks for the denominator spiral across a run of days.
func AnalyseTrend(rows []PostmasterRow) Trend {
	if len(rows) < 2 {
		return Trend{Days: len(rows), Message: "not enough days to see a trend"}
	}
	sorted := append([]PostmasterRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Date.Before(sorted[j].Date) })

	first, last := sorted[0], sorted[len(sorted)-1]
	t := Trend{Days: len(sorted)}

	if first.InboxedEstimate > 0 {
		t.InboxedChange = float64(last.InboxedEstimate) / float64(first.InboxedEstimate)
	}
	t.RateChange = last.SpamRate - first.SpamRate

	firstComplaints := first.ToMetrics().Complaints
	lastComplaints := last.ToMetrics().Complaints

	if t.InboxedChange > 0 && t.InboxedChange < CollapseThreshold && lastComplaints <= firstComplaints {
		t.DenominatorCollapse = true
		t.Message = fmt.Sprintf(
			"denominator collapse over %d days: inboxed volume fell to %.0f%% of where it started (%d to %d) while complaints went from %d to %d. "+
				"The rate moved from %.3f%% to %.3f%% on unchanged behaviour -- the sending did not get worse, the denominator got smaller. "+
				"A breaker watching raw complaint counts sees nothing here.",
			t.Days, t.InboxedChange*100, first.InboxedEstimate, last.InboxedEstimate,
			firstComplaints, lastComplaints, first.SpamRate*100, last.SpamRate*100)
		return t
	}

	t.Message = fmt.Sprintf("over %d days: inboxed volume at %.0f%% of the start, spam rate %.3f%% to %.3f%%",
		t.Days, t.InboxedChange*100, first.SpamRate*100, last.SpamRate*100)
	return t
}
