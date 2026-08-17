package service

import (
	"bufio"
	_ "embed"
	"strings"
)

//go:embed data/common_passwords.txt
var commonPasswordsRaw string

// commonPasswords is a case-insensitive exact-match lookup of well-known
// weak/breached passwords (RockYou-style top entries, keyboard-walk
// patterns, and generic weak picks), loaded once at package init. Matched
// as a whole-string comparison, not substring/fuzzy, per NIST 800-63B
// guidance on rejecting known-compromised secrets.
var commonPasswords = func() map[string]struct{} {
	m := make(map[string]struct{})
	sc := bufio.NewScanner(strings.NewReader(commonPasswordsRaw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		m[strings.ToLower(line)] = struct{}{}
	}
	return m
}()

func isCommonPassword(pw string) bool {
	_, ok := commonPasswords[strings.ToLower(pw)]
	return ok
}
