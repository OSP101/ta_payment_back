package service

// announce_text.go turns a formatted announcement body back into plain text.
//
// The body is stored as text with a deliberately tiny markup: **bold**,
// *italic*, "- " lists, "1. " lists, [label](url), and a ":::center" /
// ":::right" line that aligns the paragraph after it. The screen renders that
// markup; this file only ever REMOVES it.
//
// That asymmetry is the point. Styling lives in exactly one place — the React
// renderer — so the email and the notification preview cannot drift away from
// what the reader saw on screen, because they never try to reproduce it. They
// show the same words with the decoration taken off.

import (
	"regexp"
	"strings"
)

var (
	// [ข้อความ](https://…) → ข้อความ (https://…), so a link survives as
	// something a reader can still copy out of a plain-text email.
	mdLinkRE = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	// Emphasis markers, applied longest-first: *** before ** before *. Run in
	// any other order and the leftover stars of the longer marker survive into
	// the mail as literal text.
	boldItalRE = regexp.MustCompile(`\*\*\*([^*\n]+)\*\*\*`)
	boldRE     = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	italicRE   = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
	alignRE    = regexp.MustCompile(`(?m)^:::(center|right|left)\s*$`)
)

// announceBodyPlain strips the markup, keeping the words and the URLs.
//
// List bullets are KEPT: "- " and "1. " read correctly as plain text and are
// most of what an announcement's structure carries. Alignment lines are
// dropped entirely — there is no such thing in a plain-text mail body.
func announceBodyPlain(body string) string {
	s := alignRE.ReplaceAllString(body, "")
	s = mdLinkRE.ReplaceAllString(s, "$1 ($2)")
	s = boldItalRE.ReplaceAllString(s, "$1")
	s = boldRE.ReplaceAllString(s, "$1")
	s = italicRE.ReplaceAllString(s, "$1$2")
	// The alignment lines leave blank lines behind; collapse runs of three or
	// more newlines so the text does not arrive full of holes.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
