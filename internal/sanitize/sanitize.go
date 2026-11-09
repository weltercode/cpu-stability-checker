// Package sanitize removes the machine's personal identity from
// artifacts meant to leave the machine (R09: a bug-report bundle the
// user can post publicly).
//
// The rule is deliberately simple and auditable: the identity comes
// from the operating system RIGHT NOW (hostname, current user, the
// user's home directory), never from a curated blocklist that would
// silently be incomplete. Every text artifact destined for the bundle
// is scrubbed with these rules; the scrubber is case-insensitive
// because Windows hostnames, usernames and paths are, and longer
// needles replace first so the home directory does not leave its user
// segment behind as a "mystery" username.
//
// What the rules do NOT promise (and the bundle manifest must say so):
// other identifying content — a share name, a custom volume label, an
// event-log record id — is not personal identity in the sense this
// tool collects, and stays. The manifest states exactly which
// identity fields were replaced so the user can review before posting.
package sanitize

import (
	"os"
	"os/user"
	"sort"
	"strings"
)

// Tokens written in place of identity. Bracketed and lowercase: they
// read as editorial marks, and they can never themselves match a real
// hostname (hostnames cannot contain brackets).
const (
	HostToken = "[host]"
	UserToken = "[user]"
	HomeToken = "[home]"
)

// Identity is what this machine currently calls itself. An empty
// field means the OS did not answer — that field is then simply not a
// rule; the scrubber never invents an identity to replace.
type Identity struct {
	Hostname string
	Username string
	HomeDir  string
}

// FromOS reads the live identity. Like every provenance collector in
// this project it is unknown-safe: a failed lookup leaves that field
// empty and the bundle stays honest about one fewer replaced field.
func FromOS() Identity {
	id := Identity{}
	if h, err := os.Hostname(); err == nil {
		id.Hostname = h
	}
	if h, err := os.UserHomeDir(); err == nil {
		id.HomeDir = h
	}
	if u, err := user.Current(); err == nil {
		id.Username = u.Username
	}
	return id
}

// Rule is one replacement: Needle is matched case-insensitively and
// replaced with Token.
type Rule struct {
	Needle string
	Token  string
}

// Rules builds the replacement set from an identity: empties are
// dropped, duplicates collapse, and the longest needle wins ties of
// containment (home before username, which is often its last
// segment). Order is stable for reproducible manifests.
//
// Each path needle also gets its written-down forms: JSON escapes a
// backslash as \\ (every journal line is JSON, so identity inside
// strings appears escaped), and Go/log output sometimes shows paths
// with forward slashes. A scrubber that only knows the live OS form
// silently misses identity in escaped text — the test suite caught
// exactly that hole in the first revision of this file.
func Rules(id Identity) []Rule {
	pairs := []struct {
		needle, token string
	}{
		{id.HomeDir, HomeToken},
		{id.Hostname, HostToken},
		{id.Username, UserToken},
	}
	var rules []Rule
	seen := map[string]bool{}
	add := func(needle, token string) {
		if needle == "" {
			return
		}
		n := strings.ToLower(needle)
		if seen[n] {
			return
		}
		seen[n] = true
		rules = append(rules, Rule{Needle: n, Token: token})
	}
	for _, p := range pairs {
		add(p.needle, p.token)
		if strings.Contains(p.needle, `\`) {
			add(strings.ReplaceAll(p.needle, `\`, `\\`), p.token) // written as JSON text
			add(strings.ReplaceAll(p.needle, `\`, "/"), p.token)  // written with slashes
		}
	}
	sort.SliceStable(rules, func(i, j int) bool {
		return len(rules[i].Needle) > len(rules[j].Needle)
	})
	return rules
}

// Scrub applies the rules case-insensitively to one line of text.
// An empty rule set is a no-op: no identity, nothing to replace.
func Scrub(s string, rules []Rule) string {
	if len(rules) == 0 {
		return s
	}
	lower := strings.ToLower(s)
	var b strings.Builder
	i := 0
	for i < len(s) {
		match := false
		for _, r := range rules {
			if r.Needle != "" && strings.HasPrefix(lower[i:], r.Needle) {
				b.WriteString(r.Token)
				i += len(r.Needle)
				match = true
				break
			}
		}
		if !match {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// Contains reports whether any needle occurs in the text
// case-insensitively — the self-check primitive: after a bundle is
// written, every artifact must answer false here.
func Contains(s string, rules []Rule) bool {
	if len(rules) == 0 {
		return false
	}
	lower := strings.ToLower(s)
	for _, r := range rules {
		if r.Needle != "" && strings.Contains(lower, r.Needle) {
			return true
		}
	}
	return false
}
