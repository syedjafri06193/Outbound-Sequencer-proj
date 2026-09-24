package deliverability

import (
	"errors"
	"fmt"
	"strings"
)

// Sending-domain governance.
//
// If outbound burns the primary domain, invoices, contracts, support
// replies and password resets stop being delivered. That is not a
// marketing setback; it is an existential business failure, and it is slow
// and painful to recover from.
//
// So the primary domain is refused structurally: a validation error, not a
// warning.

var (
	ErrPrimaryDomain      = errors.New("deliverability: refusing to send outbound from the primary domain")
	ErrSubdomainOfPrimary = errors.New("deliverability: refusing a subdomain of the primary domain")
	ErrUnknownDomain      = errors.New("deliverability: domain is not registered for outbound")
	ErrNoAuth             = errors.New("deliverability: domain is missing required authentication")
	ErrSharedTracker      = errors.New("deliverability: domain uses a shared tracking domain")
	ErrNoWebsite          = errors.New("deliverability: domain has no resolving website")
)

// AuthStatus records the DNS-level requirements.
type AuthStatus struct {
	SPF   bool
	DKIM  bool
	DMARC bool
	// DKIMKeyBits must be at least 1024 -- Yahoo's published minimum.
	DKIMKeyBits int
	// DMARCPolicy is none, quarantine or reject. Bulk senders need at
	// least p=none.
	DMARCPolicy string
	// ForwardDNS and ReverseDNS must both resolve; a missing PTR is a
	// reject, not a spam-folder placement.
	ForwardDNS bool
	ReverseDNS bool
}

// SendingDomain is a domain registered for outbound.
type SendingDomain struct {
	ID   string
	Name string
	// IsPrimary marks the company's real domain. Never sendable.
	IsPrimary bool
	// TrackingDomain must be unique to this sending domain. A shared
	// tracker means inheriting every other sender's reputation, and a
	// shared tracker on a blocklist takes you with it.
	TrackingDomain string
	// HasWebsite: a domain with no resolving site is itself a spam signal.
	HasWebsite bool
	Auth       AuthStatus
	// DailyCap must stay below the bulk-sender threshold unless crossing
	// it is a deliberate decision, because the classification is
	// permanent once triggered.
	DailyCap int
	// BulkClassified records that this domain has already crossed 5,000
	// messages a day to Gmail. Once true it never goes back to false:
	// the classification does not expire when volume decreases.
	BulkClassified bool
}

// Registry holds the outbound domains and the primary domain to protect.
type Registry struct {
	primary   string
	domains   map[string]*SendingDomain
	byTracker map[string]string
}

func NewRegistry(primaryDomain string) *Registry {
	return &Registry{
		primary:   normalizeDomain(primaryDomain),
		domains:   make(map[string]*SendingDomain),
		byTracker: make(map[string]string),
	}
}

func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
}

// Register validates and adds a sending domain.
func (r *Registry) Register(d *SendingDomain) error {
	name := normalizeDomain(d.Name)
	if name == "" {
		return fmt.Errorf("deliverability: domain name is empty")
	}

	if err := r.checkNotPrimary(name); err != nil {
		return err
	}
	if d.IsPrimary {
		return fmt.Errorf("%w: %s is marked primary", ErrPrimaryDomain, name)
	}

	if !d.Auth.SPF && !d.Auth.DKIM {
		return fmt.Errorf("%w: %s has neither SPF nor DKIM", ErrNoAuth, name)
	}
	if d.Auth.DKIM && d.Auth.DKIMKeyBits < 1024 {
		return fmt.Errorf("%w: %s has a %d-bit DKIM key, below Yahoo's 1024-bit minimum",
			ErrNoAuth, name, d.Auth.DKIMKeyBits)
	}
	if !d.Auth.ForwardDNS || !d.Auth.ReverseDNS {
		return fmt.Errorf("%w: %s is missing forward or reverse DNS, which is a reject rather than a spam placement",
			ErrNoAuth, name)
	}
	if !d.HasWebsite {
		return fmt.Errorf("%w: %s", ErrNoWebsite, name)
	}

	tracker := normalizeDomain(d.TrackingDomain)
	if tracker == "" {
		return fmt.Errorf("%w: %s has no tracking domain of its own", ErrSharedTracker, name)
	}
	if owner, taken := r.byTracker[tracker]; taken && owner != name {
		return fmt.Errorf("%w: %s is already used by %s", ErrSharedTracker, tracker, owner)
	}
	if err := r.checkNotPrimary(tracker); err != nil {
		return fmt.Errorf("tracking domain: %w", err)
	}

	if d.DailyCap <= 0 || d.DailyCap >= BulkSenderDailyThreshold {
		return fmt.Errorf(
			"deliverability: daily cap %d for %s must be between 1 and %d; crossing the bulk-sender threshold permanently changes this domain's obligations",
			d.DailyCap, name, BulkSenderDailyThreshold-1)
	}

	d.Name = name
	d.TrackingDomain = tracker
	r.domains[name] = d
	r.byTracker[tracker] = name
	return nil
}

// checkNotPrimary refuses the primary domain and any subdomain of it.
//
// Subdomains are refused because subdomain reputation is not fully
// isolated from the parent, and isolation is the entire point. An operator
// reaching for `mail.yourcompany.com` has understood the goal and picked
// the one shortcut that defeats it.
func (r *Registry) checkNotPrimary(name string) error {
	if r.primary == "" {
		return nil
	}
	if name == r.primary {
		return fmt.Errorf("%w: %s", ErrPrimaryDomain, name)
	}
	if strings.HasSuffix(name, "."+r.primary) {
		return fmt.Errorf("%w: %s is under %s; subdomain reputation is not isolated from the parent",
			ErrSubdomainOfPrimary, name, r.primary)
	}
	return nil
}

// Get returns a registered sending domain.
func (r *Registry) Get(name string) (*SendingDomain, error) {
	d, ok := r.domains[normalizeDomain(name)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownDomain, name)
	}
	return d, nil
}

// ValidateForSending is the check the sequence validator calls.
func (r *Registry) ValidateForSending(name string) error {
	if err := r.checkNotPrimary(normalizeDomain(name)); err != nil {
		return err
	}
	_, err := r.Get(name)
	return err
}

// RecordDailyVolume applies the bulk-sender classification if crossed.
//
// One-way: the classification does not expire when volume decreases, so a
// single enthusiastic blast permanently changes the domain's obligations.
func (r *Registry) RecordDailyVolume(name string, gmailMessages int) (newlyClassified bool) {
	d, ok := r.domains[normalizeDomain(name)]
	if !ok {
		return false
	}
	if gmailMessages >= BulkSenderDailyThreshold && !d.BulkClassified {
		d.BulkClassified = true
		return true
	}
	return false
}

// BulkRequirements lists what a bulk-classified domain must satisfy.
func (d *SendingDomain) BulkRequirements() []string {
	var missing []string
	if !d.BulkClassified {
		return nil
	}
	if d.Auth.DMARCPolicy == "" {
		missing = append(missing, "DMARC policy (minimum p=none)")
	}
	if !d.Auth.SPF || !d.Auth.DKIM {
		// Google signals full alignment on both is likely to become
		// mandatory, so a bulk domain with only one is on borrowed time.
		missing = append(missing, "both SPF and DKIM (alignment on both is expected to become mandatory)")
	}
	if d.Auth.DKIMKeyBits < 1024 {
		missing = append(missing, "DKIM key of at least 1024 bits")
	}
	return missing
}

// Domains lists every registered sending domain.
func (r *Registry) Domains() []*SendingDomain {
	out := make([]*SendingDomain, 0, len(r.domains))
	for _, d := range r.domains {
		out = append(out, d)
	}
	return out
}
