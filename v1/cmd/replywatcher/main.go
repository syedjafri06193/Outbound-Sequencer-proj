// Command replywatcher classifies an inbound message read from stdin and
// prints what the engine would do about it.
//
// Input is an RFC 5322-ish message: headers, a blank line, then the body.
// The point of having this as a binary is that out-of-office handling is
// the thing that visibly separates good sequencers from bad ones, and being
// able to paste a real auto-reply in and see "pause until 17 March, not
// counted as a reply" is how you find out whether it works on YOUR mail.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/syedjafri06193/Outbound-Sequencer-proj/internal/reply"
)

func main() {
	received := flag.String("received", "", "received date (YYYY-MM-DD); defaults to today")
	flag.Parse()

	at := time.Now()
	if *received != "" {
		d, err := time.Parse("2006-01-02", *received)
		if err != nil {
			fmt.Fprintln(os.Stderr, "replywatcher:", err)
			os.Exit(1)
		}
		at = d
	}

	msg, err := parse(os.Stdin, at)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replywatcher:", err)
		os.Exit(1)
	}

	report(msg)
}

// parse reads headers until a blank line, then the body.
//
// Folded header continuation lines are joined, because References in
// particular is routinely wrapped and a parser that drops the continuation
// silently loses every ancestor but the first.
func parse(r io.Reader, received time.Time) (reply.InboundMessage, error) {
	br := bufio.NewReader(r)
	headers := map[string]string{}
	var last string

	for {
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			break
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && last != "" {
			headers[last] += " " + strings.TrimSpace(trimmed)
		} else if i := strings.Index(trimmed, ":"); i > 0 {
			last = trimmed[:i]
			headers[last] = strings.TrimSpace(trimmed[i+1:])
		}
		if err != nil {
			break
		}
	}

	body, _ := io.ReadAll(br)

	msg := reply.InboundMessage{
		Headers:    headers,
		Body:       string(body),
		ReceivedAt: received,
	}
	msg.MessageID = msg.Header("Message-ID")
	msg.From = msg.Header("From")
	msg.Subject = msg.Header("Subject")
	msg.References = reply.ParseReferences(msg.Header("References"))
	if v := msg.Header("In-Reply-To"); v != "" {
		msg.InReplyTo = reply.ParseReferences(v)
	}
	return msg, nil
}

func report(msg reply.InboundMessage) {
	fmt.Println("DETECTION")
	fmt.Println("---------")
	fmt.Printf("  auto-reply (from headers): %v\n", reply.IsAutoReply(msg))
	fmt.Printf("  bulk or list mail:         %v\n", reply.IsBulkOrList(msg))
	fmt.Printf("  threading on:              In-Reply-To %v, References %v, never the subject\n",
		msg.InReplyTo, msg.References)

	d := reply.NewClassifier().Classify(msg)

	fmt.Println("\nCLASSIFICATION")
	fmt.Println("--------------")
	fmt.Printf("  class:            %s\n", d.Class)
	fmt.Printf("  counts as reply:  %v\n", d.CountsAsReply)
	fmt.Printf("  stop sequence:    %v\n", d.StopSequence)
	if !d.PauseUntil.IsZero() {
		fmt.Printf("  pause until:      %s\n", d.PauseUntil.Format("2006-01-02 15:04"))
	}
	if !d.ReEnrollAt.IsZero() {
		fmt.Printf("  re-enrol at:      %s\n", d.ReEnrollAt.Format("2006-01-02"))
	}
	if d.SuppressGlobally {
		fmt.Printf("  suppress:         GLOBALLY, reason %q\n", d.SuppressReason)
	}
	if d.NotifyRep {
		fmt.Println("  notify rep:       yes")
	}
	if d.FlagForReview {
		fmt.Println("  flag for review:  yes")
	}
	if d.FlagAccountForNewContact {
		fmt.Println("  account:          find a new contact")
	}
	fmt.Printf("\n  %s\n", d.Rationale)

	if d.Class == reply.ReplyOutOfOffice {
		fmt.Println("\nNote: an out-of-office is NOT counted as a reply. Treating it as")
		fmt.Println("engagement stops the sequence for someone who never saw the email,")
		fmt.Println("which is the most common way a sequencer wastes a prospect.")
	}
}
