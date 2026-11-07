//go:build windows

package whea

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The event service does not tolerate an unbalanced predicate: it rejects
// the whole query with exit 15001 ("syntax error at position <near end>"),
// and the monitoring then reads as a Windows problem while it is our own
// typo. This incident cost WHEA coverage on every run of the owner's
// machine until the missing "]" in the third provider predicate was
// fixed - so the exact query text is pinned here, without needing the
// event log at all.
func TestWheaQueryTextIsWellFormed(t *testing.T) {
	pred := regexp.MustCompile(`Provider\[@Name='[^']+'\]`)
	for _, tc := range []struct {
		name  string
		xpath string
	}{
		{name: "cursor read", xpath: xpath(wheaCursorFilter(357974))},
		{name: "newest records", xpath: xpath(wheaFilter)},
		{name: "probe", xpath: xpath(probeFilter)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if opens, closes := strings.Count(tc.xpath, "("), strings.Count(tc.xpath, ")"); opens != closes {
				t.Errorf("unbalanced parentheses (%d/%d) in %s", opens, closes, tc.xpath)
			}
			if opens, closes := strings.Count(tc.xpath, "["), strings.Count(tc.xpath, "]"); opens != closes {
				t.Errorf("unbalanced brackets (%d/%d) in %s", opens, closes, tc.xpath)
			}
			if tc.name != "probe" {
				if n := len(pred.FindAllString(tc.xpath, -1)); n != 3 {
					t.Errorf("want 3 complete provider predicates, found %d in %s", n, tc.xpath)
				}
			}
		})
	}
}

// The same lesson verified against the real event service: every query the
// monitor issues must be ACCEPTED (a rejected one is reported as a syntax
// error, which we now know means our text, not the machine). Skipped when
// wevtutil is absent. A deliberately broken query is checked first: it
// proves this test can fail, instead of passing because the service is
// unhappy for some other reason.
func TestRealEventLogAcceptsEveryMonitorQuery(t *testing.T) {
	q, ok := newQuerier()
	if !ok {
		t.Skip("wevtutil is not available on this machine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	wq, ok := q.(wevtutilQuerier)
	if !ok {
		t.Skipf("unexpected querier type %T", q)
	}
	if _, err := wq.run(ctx, "Provider[@Name='Broken'", 1, true); err == nil {
		t.Fatal("control: an unbalanced predicate must be rejected - if it is not, this test proves nothing")
	}

	if _, err := wq.newest(ctx); err != nil {
		t.Errorf("newest probe rejected: %v", err)
	}
	if _, err := wq.oldest(ctx); err != nil {
		t.Errorf("oldest probe rejected: %v", err)
	}
	if _, err := wq.query(ctx, 0, 5); err != nil {
		t.Errorf("cursor query rejected: %v", err)
	}
	if _, err := wq.recent(ctx, 5); err != nil {
		t.Errorf("recent query rejected: %v", err)
	}
}
