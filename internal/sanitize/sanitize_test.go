package sanitize

import (
	"strings"
	"testing"
)

// The rules come from the OS identity, and the scrubber's job is to
// be simple and provable. These tests pin the exact semantics the
// bundle promises: case-insensitive matching (Windows identity is),
// longest-needle-first (the home path usually ENDS with the username;
// replacing the short username first would shred the path into
// "[home-with-[user]-inside]" nonsense), dedup, and unknown-safety
// (an identity field the OS did not answer is simply not a rule).

func TestRulesShape(t *testing.T) {
	id := Identity{Hostname: "BOX", Username: "wel", HomeDir: `C:\Users\wel`}
	rules := Rules(id)
	// home raw + escaped + slashed, host, user = 5 needles, longest
	// first: the escaped home form is the longest of them.
	if len(rules) != 5 {
		t.Fatalf("rules = %q, want 5", ruleStrings(rules))
	}
	if rules[0].Token != HomeToken || len(rules[0].Needle) != len(`C:\\Users\\wel`) {
		t.Errorf("longest rule should be the escaped home path, got %+v", rules[0])
	}
	tokens := map[string]int{}
	for _, r := range rules {
		tokens[r.Token]++
	}
	if tokens[HomeToken] != 3 || tokens[HostToken] != 1 || tokens[UserToken] != 1 {
		t.Errorf("token spread wrong: %+v (%q)", tokens, ruleStrings(rules))
	}

	// empty fields never become rules; duplicates collapse.
	if got := Rules(Identity{}); len(got) != 0 {
		t.Errorf("empty identity must yield no rules, got %+v", got)
	}
	dup := Rules(Identity{Hostname: "same", Username: "SAME", HomeDir: ""})
	if len(dup) != 1 {
		t.Errorf("case-duplicate identity must collapse, got %q", ruleStrings(dup))
	}
}

func TestScrubSemantics(t *testing.T) {
	rules := Rules(Identity{Hostname: "TestBox", Username: "wel", HomeDir: `C:\Users\wel`})

	got := Scrub(`run started in C:\USERS\WEL\logs by TestBox for WEL`, rules)
	want := `run started in [home]\logs by [host] for [user]`
	if got != want {
		t.Errorf("case-insensitive scrub:\n got %q\nwant %q", got, want)
	}

	// The username inside the home path must not leak a second time:
	// longest-first consumed the whole path.
	if strings.Contains(Scrub(`C:\Users\wel`, rules), "wel") {
		t.Error("username leaked through the home path")
	}

	// No rules, no change (unknown identity stays unknown, and text
	// on a platform where the OS refused identity answers must not be
	// silently rewritten).
	if Scrub("C:\\Users\\wel", nil) != "C:\\Users\\wel" {
		t.Error("empty rule set must be a no-op")
	}

	// The written-down forms: every journal line is JSON, so the
	// identity inside a string appears with ESCAPED backslashes; and
	// some logs show slashed paths. Missing either form silently
	// leaves identity in the bundle — this is the hole the bundle
	// e2e caught in the first revision.
	gotJSON := Scrub(`{"msg":"at C:\\Users\\wel\\x on TeStBoX"}`, rules)
	if want := `{"msg":"at [home]\\x on [host]"}`; gotJSON != want {
		t.Errorf("escaped JSON form:\n got %q\nwant %q", gotJSON, want)
	}
	gotSlash := Scrub(`opened C:/Users/wel/file on box.testbox.local`, rules)
	if want := `opened [home]/file on box.[host].local`; gotSlash != want {
		t.Errorf("slashed form:\n got %q\nwant %q", gotSlash, want)
	}

	// Contains is the self-check primitive: matches whatever case AND
	// the written-down forms (a self-check blind to the escaped form
	// would certify an escaping leak as clean).
	if !Contains("ON TESTBOX RUNNING", rules) {
		t.Error("Contains must match case-insensitively")
	}
	if !Contains(`{"m":"C:\\USERS\\WEL\\x"}`, rules) {
		t.Error("Contains must catch the escaped form")
	}
	if Contains("everything [host] [user] [home] here", rules) {
		t.Error("tokens themselves must never count as identity")
	}
}

func ruleStrings(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Needle+"->"+r.Token)
	}
	return out
}
