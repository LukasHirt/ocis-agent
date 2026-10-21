package api

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
)

// follow reads every page of query (which must not carry a cursor) and
// returns the seqs and the cursors in order.
func follow(t *testing.T, f *fixture, query string) (got []int64, cursors []string) {
	t.Helper()
	cursor := ""
	for range 1000 {
		q := query
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		l := getAudit(t, f, q)
		got = append(got, seqs(l)...)
		if l.NextCursor == nil {
			return got, cursors
		}
		cursor = *l.NextCursor
		cursors = append(cursors, cursor)
	}
	t.Fatalf("%s: no end after 1000 pages", query)
	return nil, nil
}

func TestAuditListsNewestFirst(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 1200, nil)
	got, cursors := follow(t, f, "?order=desc&limit=200")
	if len(got) != 1200 || len(cursors) != 5 {
		t.Fatalf("events=%d pages=%d", len(got), len(cursors)+1)
	}
	for i, s := range got {
		if s != int64(1200-i) {
			t.Fatalf("event %d has seq %d", i, s)
		}
	}
	for i, c := range cursors {
		if want := "d" + strconv.Itoa(1200-200*(i+1)+1); c != want {
			t.Errorf("cursor %d = %s, want %s", i, c, want)
		}
	}
	// default limit
	if l := getAudit(t, f, "?order=desc"); len(l.Events) != 50 || seqs(l)[0] != 1200 || l.NextCursor == nil || *l.NextCursor != "d1151" {
		t.Fatalf("default page: %d events, cursor %v", len(l.Events), l.NextCursor)
	}
	// exact fit: the last page has no next cursor
	if l := getAudit(t, f, "?order=desc&limit=200&cursor=d201"); len(l.Events) != 200 || l.NextCursor != nil || seqs(l)[199] != 1 {
		t.Fatalf("last page: %d events, cursor %v", len(l.Events), l.NextCursor)
	}
	// order=asc is the default
	asc, ascCursors := follow(t, f, "?order=asc&limit=200")
	def, defCursors := follow(t, f, "?limit=200")
	if len(asc) != 1200 || asc[0] != 1 || len(def) != 1200 || ascCursors[0] != "200" || defCursors[0] != "200" {
		t.Fatalf("asc: %d events, cursors %v / %v", len(asc), ascCursors, defCursors)
	}
}

func TestAuditFilterScanIsBoundedNewestFirst(t *testing.T) {
	f := newFixture(t)
	// the only match sits behind 6000 newer non-matching records
	seedAudit(t, f.st, aliceID.UserID, 6001, func(i int, e *audit.Event) {
		if i == 0 {
			e.Tool = "files.move"
		}
	})
	l := getAudit(t, f, "?order=desc&tool=files.move")
	if len(l.Events) != 0 || l.NextCursor == nil || *l.NextCursor != "d1002" {
		t.Fatalf("first scan: %d events, cursor %v (want empty page, cursor d1002)", len(l.Events), l.NextCursor)
	}
	l = getAudit(t, f, "?order=desc&tool=files.move&cursor=d1002")
	if got := seqs(l); len(got) != 1 || got[0] != 1 || l.NextCursor != nil {
		t.Fatalf("second scan: %v cursor %v", got, l.NextCursor)
	}
}

func TestAuditKindFilter(t *testing.T) {
	f := newFixture(t)
	ks := []string{audit.KindApprovalDecided, audit.KindToolCall, audit.KindJobFinished}
	seedAudit(t, f.st, aliceID.UserID, 1200, func(i int, e *audit.Event) {
		e.Kind = ks[i%3]
		if i%3 == 1 {
			e.Tool = "files.read"
		}
	})
	for _, order := range []string{"asc", "desc"} {
		got, _ := follow(t, f, "?limit=7&order="+order+"&kind="+audit.KindApprovalDecided)
		if len(got) != 400 {
			t.Fatalf("%s: %d events, want 400", order, len(got))
		}
		for i, s := range got {
			want := int64(1 + 3*i)
			if order == "desc" {
				want = int64(1198 - 3*i)
			}
			if s != want {
				t.Fatalf("%s: event %d has seq %d, want %d", order, i, s, want)
			}
		}
		// kind combines with the other filters
		if got, _ := follow(t, f, "?limit=200&order="+order+"&kind="+audit.KindToolCall+"&tool=files.read"); len(got) != 400 {
			t.Errorf("%s: kind+tool: %d events", order, len(got))
		}
		if got, _ := follow(t, f, "?order="+order+"&kind="+audit.KindToolCall+"&tool=files.write"); len(got) != 0 {
			t.Errorf("%s: kind+other tool: %v", order, got)
		}
		// a known kind that never occurs: no events, end of log
		if l := getAudit(t, f, "?order="+order+"&kind="+audit.KindUnenrolled); len(l.Events) != 0 || l.NextCursor != nil {
			t.Errorf("%s: absent kind: %v cursor %v", order, seqs(l), l.NextCursor)
		}
	}
	// the event carries its kind
	l := getAudit(t, f, "?limit=1&kind="+audit.KindJobFinished)
	if len(l.Events) != 1 || l.Events[0]["kind"] != audit.KindJobFinished {
		t.Fatalf("kind page: %v", l.Events)
	}
}

func TestAuditKindFilterPagesThroughEmptyScans(t *testing.T) {
	f := newFixture(t)
	// one unenrolled event at each end, 12000 others between them
	seedAudit(t, f.st, aliceID.UserID, 12002, func(i int, e *audit.Event) {
		if i == 0 || i == 12001 {
			e.Kind = audit.KindUnenrolled
		}
	})
	type page struct {
		seqs   string
		cursor string // "" is null
	}
	for order, want := range map[string][]page{
		"asc":  {{"[1]", "5000"}, {"[]", "10000"}, {"[12002]", ""}},
		"desc": {{"[12002]", "d7003"}, {"[]", "d2003"}, {"[1]", ""}},
	} {
		var got []page
		cursor := ""
		for range 10 {
			q := "?limit=1&order=" + order + "&kind=" + audit.KindUnenrolled
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			l := getAudit(t, f, q)
			p := page{seqs: fmt.Sprint(seqs(l))}
			if l.NextCursor != nil {
				p.cursor = *l.NextCursor
			}
			got = append(got, p)
			if l.NextCursor == nil {
				break
			}
			cursor = *l.NextCursor
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: pages %v, want %v", order, got, want)
		}
	}
}

func TestAuditCursorsAreBoundToTheirOrder(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 10, nil)
	asc := getAudit(t, f, "?limit=3")
	desc := getAudit(t, f, "?limit=3&order=desc")
	if *asc.NextCursor != "3" || *desc.NextCursor != "d8" {
		t.Fatalf("cursors %s %s", *asc.NextCursor, *desc.NextCursor)
	}
	wantError(t, f.do(http.MethodGet, auditPath+"?order=desc&cursor="+*asc.NextCursor, ""), http.StatusBadRequest, "invalid_request")
	wantError(t, f.do(http.MethodGet, auditPath+"?cursor="+*desc.NextCursor, ""), http.StatusBadRequest, "invalid_request")
	wantError(t, f.do(http.MethodGet, auditPath+"?order=asc&cursor="+*desc.NextCursor, ""), http.StatusBadRequest, "invalid_request")
	// each continues in its own order
	if got := seqs(getAudit(t, f, "?limit=3&order=desc&cursor=d8")); len(got) != 3 || got[0] != 7 || got[2] != 5 {
		t.Fatalf("desc continuation: %v", got)
	}
}

func TestAuditRefusesForeignCursors(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 5, nil)
	seedAudit(t, f.st, bobID.UserID, 9, nil)
	f.bearer = otherToken
	bobDesc := getAudit(t, f, "?limit=1&order=desc")
	if *bobDesc.NextCursor != "d9" {
		t.Fatalf("bob's cursor %s", *bobDesc.NextCursor)
	}
	f.bearer = goodToken
	// alice's log ends at 5: bob's cursor names no record of hers
	for _, q := range []string{
		"?order=desc&cursor=" + *bobDesc.NextCursor, "?order=desc&cursor=d6",
		"?order=desc&tool=files.read&cursor=d9", "?kind=tool_call&order=desc&cursor=d7",
	} {
		wantError(t, f.do(http.MethodGet, auditPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
	// her own cursors still work at the edges
	if l := getAudit(t, f, "?order=desc&cursor=d1"); len(l.Events) != 0 || l.NextCursor != nil {
		t.Errorf("before her first record: %v", seqs(l))
	}
	if got := seqs(getAudit(t, f, "?order=desc&cursor=d5")); len(got) != 4 || got[0] != 4 {
		t.Errorf("before her last record: %v", got)
	}
	// an asc cursor must name a record of her log too: bob's seqs past
	// her end are refused, filtered or not
	f.bearer = otherToken
	bobAsc := getAudit(t, f, "?limit=7")
	f.bearer = goodToken
	if *bobAsc.NextCursor != "7" {
		t.Fatalf("bob's asc cursor %s", *bobAsc.NextCursor)
	}
	for _, q := range []string{
		"?cursor=" + *bobAsc.NextCursor, "?cursor=6", "?cursor=1000", "?order=asc&cursor=9",
		"?tool=files.read&cursor=7", "?kind=tool_call&cursor=6",
	} {
		wantError(t, f.do(http.MethodGet, auditPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
	// her last record is a valid asc cursor: an empty last page
	if l := getAudit(t, f, "?cursor=5"); len(l.Events) != 0 || l.NextCursor != nil {
		t.Errorf("after her last record: %v cursor %v", seqs(l), l.NextCursor)
	}
	// a user without any audit record has no valid cursor in either order
	f2 := newFixture(t)
	wantError(t, f2.do(http.MethodGet, auditPath+"?order=desc&cursor=d1", ""), http.StatusBadRequest, "invalid_request")
	wantError(t, f2.do(http.MethodGet, auditPath+"?cursor=1", ""), http.StatusBadRequest, "invalid_request")
}

func TestAuditAscCursorMustNameAnExistingEvent(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 7, nil)
	// unknown: past the end of the log
	for _, q := range []string{"?cursor=8", "?cursor=999999", "?order=asc&cursor=8", "?limit=1&kind=tool_call&cursor=8"} {
		wantError(t, f.do(http.MethodGet, auditPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
	// a cursor the server issued works and continues after it
	p1 := getAudit(t, f, "?limit=3")
	if p1.NextCursor == nil || *p1.NextCursor != "3" {
		t.Fatalf("first page cursor %v", p1.NextCursor)
	}
	p2 := getAudit(t, f, "?limit=3&cursor="+*p1.NextCursor)
	if got := seqs(p2); len(got) != 3 || got[0] != 4 || got[2] != 6 || p2.NextCursor == nil || *p2.NextCursor != "6" {
		t.Fatalf("second page %v cursor %v", got, p2.NextCursor)
	}
	// the last page has a null cursor
	p3 := getAudit(t, f, "?limit=3&cursor="+*p2.NextCursor)
	if got := seqs(p3); len(got) != 1 || got[0] != 7 || p3.NextCursor != nil {
		t.Fatalf("last page %v cursor %v", got, p3.NextCursor)
	}
	// any existing seq is a valid cursor (the log is append-only), the
	// newest one gives an empty last page
	if got := seqs(getAudit(t, f, "?cursor=1")); len(got) != 6 || got[0] != 2 {
		t.Fatalf("after 1: %v", got)
	}
	if l := getAudit(t, f, "?cursor=7"); len(l.Events) != 0 || l.NextCursor != nil {
		t.Fatalf("after the newest: %v cursor %v", seqs(l), l.NextCursor)
	}
	// a cursor stays valid while the log grows
	seedAudit(t, f.st, aliceID.UserID, 2, nil)
	if got := seqs(getAudit(t, f, "?cursor=7")); len(got) != 2 || got[0] != 8 {
		t.Fatalf("after growth: %v", got)
	}
}

func TestAuditRejectsUnknownOrderAndKind(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 2, nil)
	for _, q := range []string{
		"?order=ASC", "?order=newest", "?order=asc&order=desc", "?order=desc%20",
		"?kind=nope", "?kind=Tool_call", "?kind=tool", "?kind=tool_call&kind=job_finished", "?kind=tool_call%00",
		"?order=desc&cursor=d", "?order=desc&cursor=d0", "?order=desc&cursor=d01", "?order=desc&cursor=D1",
		"?order=desc&cursor=dd1", "?order=desc&cursor=d-1", "?order=desc&cursor=d1234567890123456789",
		"?order=desc&cursor=d1&cursor=d2",
	} {
		wantError(t, f.do(http.MethodGet, auditPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
	// the export has neither parameter
	for _, q := range []string{"?order=desc", "?kind=tool_call"} {
		wantError(t, f.do(http.MethodGet, exportPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
}
