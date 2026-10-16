package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func enrolled(t *testing.T) env {
	t.Helper()
	e := newEnv(t)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e env) remoteLabels() []string {
	var l []string
	for _, tk := range e.fake.Tokens() {
		l = append(l, tk.Label)
	}
	return l
}

func TestRenewSwapsTheTokenAndDeletesTheOldOne(t *testing.T) {
	e := enrolled(t)
	old := e.token(t, userID)
	oldValue := e.fake.Tokens()[0].Value
	e.fake.Advance(40 * time.Hour)

	res, err := e.svc.Renew(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	rec := e.token(t, userID)
	if rec.Label == old.Label || rec.Label != res.Label || rec.Status != store.TokenValid || !res.OldCleaned {
		t.Fatalf("record %+v, result %+v", rec, res)
	}
	if got := rec.ExpiresAt.Sub(e.now()); got != defaultTokenTTL {
		t.Fatalf("new lifetime = %s", got)
	}
	if labels := e.remoteLabels(); len(labels) != 1 || labels[0] != rec.Label {
		t.Fatalf("remote tokens = %v, want only the new one", labels)
	}
	cr, err := e.svc.AppCreds(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.Me(bg, cr); err != nil {
		t.Fatalf("the stored token must work: %v", err)
	}
	if _, err := e.client.Me(bg, ocisclient.AppToken("einstein", ocisclient.Secret(oldValue))); !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("the old token must be dead, got %v", err)
	}
	var open, closed int
	for _, l := range e.labels(t, userID) {
		if l.DeletedAt == nil {
			open++
			if l.Label != rec.Label {
				t.Fatalf("only the current label may stay open: %+v", l)
			}
		} else {
			closed++
		}
	}
	if open != 1 || closed != 1 {
		t.Fatalf("labels open=%d closed=%d", open, closed)
	}
	if k := kinds(e.events(t, userID)); k != "token_created,token_renewed,token_deleted" {
		t.Fatalf("audit = %s", k)
	}
	e.noLeaks(t, userID, oldValue, e.fake.Tokens()[0].Value, e.bearer.Reveal())
}

func TestRenewUsesTheAppTokenAndNeverTheBearer(t *testing.T) {
	e := enrolled(t)
	e.fake.Advance(40 * time.Hour)
	e.fake.ResetRequests()
	if _, err := e.svc.Renew(bg, userID); err != nil {
		t.Fatal(err)
	}
	reqs := e.fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, r := range reqs {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer") {
			t.Fatalf("renewal sent the OIDC bearer to %s %s", r.Method, r.Path)
		}
	}
	// the sweep too
	e.fake.ResetRequests()
	if _, err := e.svc.Sweep(bg, userID); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.fake.Requests() {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer") {
			t.Fatalf("the sweep sent the OIDC bearer to %s %s", r.Method, r.Path)
		}
	}
}

func TestRenewDueSelectsOnlyTokensPastHalfTheirLifetime(t *testing.T) {
	e := enrolled(t)
	e.fake.Advance(30 * time.Hour)
	out, err := e.svc.RenewDue(bg, 100)
	if err != nil || len(out) != 0 {
		t.Fatalf("at 30h of 72h nothing is due: %+v %v", out, err)
	}
	e.fake.Advance(10 * time.Hour)
	out, err = e.svc.RenewDue(bg, 100)
	if err != nil || len(out) != 1 || out[0].UserID != userID || out[0].Err != nil || out[0].Result.Label == "" {
		t.Fatalf("at 40h the token is due: %+v %v", out, err)
	}
	out, err = e.svc.RenewDue(bg, 100)
	if err != nil || len(out) != 0 {
		t.Fatalf("a fresh token is not due: %+v %v", out, err)
	}
}

func TestDueForRenewalListsWhatRenewDueWouldRenew(t *testing.T) {
	e := enrolled(t)
	e.fake.Advance(30 * time.Hour)
	if ids, err := e.svc.DueForRenewal(bg, 100); err != nil || len(ids) != 0 {
		t.Fatalf("at 30h of 72h nothing is due: %v %v", ids, err)
	}
	e.fake.Advance(10 * time.Hour)
	if ids, err := e.svc.DueForRenewal(bg, 100); err != nil || len(ids) != 1 || ids[0] != userID {
		t.Fatalf("at 40h the token is due: %v %v", ids, err)
	}
}

func TestRenewKeepsRenewingWhileThePolicyIsPaused(t *testing.T) {
	e := enrolled(t)
	for _, reason := range []string{store.PauseUser, store.PauseDormant} {
		_ = e.st.InTx(bg, func(tx store.Tx) error {
			_, err := tx.ClearPause(userID, store.PauseUser, store.PauseDormant)
			return err
		})
		_ = e.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(userID, reason); return err })
		e.fake.Advance(40 * time.Hour)
		if _, err := e.svc.Renew(bg, userID); err != nil {
			t.Fatalf("renewal must continue while paused (%s): %v", reason, err)
		}
		if p := e.policy(t, userID); p.PauseReason != reason {
			t.Fatalf("renewal changed the pause: %+v", p)
		}
	}
}

func TestRenewWithoutAValidTokenIsRefusedBeforeAnyRequest(t *testing.T) {
	e := newEnv(t)
	label := e.seed(t, e.ident, time.Hour)
	e.deleteRemote(t, label)
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	e.fake.ResetRequests()
	if _, err := e.svc.Renew(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.Renew(bg, "nobody"); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("unknown user = %v", err)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d request(s) sent without a valid token", n)
	}
}

func TestFailedCreateKeepsTheOldTokenAndSelfHeals(t *testing.T) {
	e := enrolled(t)
	old := e.token(t, userID)
	e.fake.Fail("POST", "/auth-app/", 503, 1)
	if _, err := e.svc.Renew(bg, userID); err == nil || errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("a 503 is a transient failure, got %v", err)
	}
	if got := e.token(t, userID); got.Label != old.Label || got.Status != store.TokenValid {
		t.Fatalf("the old token must stay: %+v", got)
	}
	if _, err := e.svc.AppCreds(bg, userID); err != nil {
		t.Fatal(err)
	}
	// the attempted label stays open (the outcome of a failed request is
	// unknown) until a sweep proves nothing exists under it
	open := 0
	for _, l := range e.labels(t, userID) {
		if l.DeletedAt == nil {
			open++
		}
	}
	if open != 2 {
		t.Fatalf("open labels = %d, want the current and the attempted one", open)
	}
	e.fake.Advance(6 * time.Minute) // past the grace period
	res, err := e.svc.Sweep(bg, userID)
	if err != nil || !res.Clean || res.Closed != 1 || res.Deleted != 0 {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	for _, l := range e.labels(t, userID) {
		if l.DeletedAt == nil && l.Label != old.Label {
			t.Fatalf("the attempted label must be closed: %+v", l)
		}
	}
}

func TestFailedVerificationDeletesTheNewTokenAndKeepsTheOldOne(t *testing.T) {
	e := enrolled(t)
	old := e.token(t, userID)
	e.fake.Fail("GET", "/graph/v1.0/me", 500, 1) // the verification call
	if _, err := e.svc.Renew(bg, userID); err == nil {
		t.Fatal("expected an error")
	}
	if got := e.token(t, userID); got.Label != old.Label {
		t.Fatalf("the old token must stay: %+v", got)
	}
	if labels := e.remoteLabels(); len(labels) != 1 || labels[0] != old.Label {
		t.Fatalf("the unverified token must be deleted, remote = %v", labels)
	}
	for _, l := range e.labels(t, userID) {
		if l.Label != old.Label && l.DeletedAt == nil {
			t.Fatalf("the discarded label must be closed: %+v", l)
		}
	}
}

func TestACrashBetweenCreateAndSwapLeavesOnlyASweepableOrphan(t *testing.T) {
	e := enrolled(t)
	cur := e.token(t, userID)
	// what a crashed renewal leaves: a recorded label and a live token, no swap
	orphan := e.svc.newLabel()
	if err := e.st.InTx(bg, func(tx store.Tx) error { return tx.AddTokenLabel(userID, orphan) }); err != nil {
		t.Fatal(err)
	}
	cr, _ := e.svc.AppCreds(bg, userID)
	if _, err := e.client.CreateToken(bg, cr, time.Hour, orphan); err != nil {
		t.Fatal(err)
	}
	if got := e.token(t, userID); got.Label != cur.Label {
		t.Fatal("the stored token must be unchanged")
	}
	res, err := e.svc.Sweep(bg, userID)
	if err != nil || res.Deleted != 1 || !res.Clean || res.Unknown != 0 {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if labels := e.remoteLabels(); len(labels) != 1 || labels[0] != cur.Label {
		t.Fatalf("remote = %v, only the current token may remain", labels)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("our own orphan is not an anomaly: %+v", p)
	}
	if k := kinds(e.events(t, userID)); !strings.HasSuffix(k, "token_deleted") {
		t.Fatalf("audit = %s", k)
	}
}

func TestAFailedDeleteIsRetriedBySweep(t *testing.T) {
	e := enrolled(t)
	oldLabel := e.token(t, userID).Label
	e.fake.Advance(40 * time.Hour)
	e.fake.Fail("DELETE", "/auth-app/", 503, 1)
	res, err := e.svc.Renew(bg, userID)
	if err != nil {
		t.Fatalf("a failed delete after the swap is not a renewal failure: %v", err)
	}
	if res.OldCleaned {
		t.Fatal("OldCleaned must be false")
	}
	if n := len(e.fake.Tokens()); n != 2 {
		t.Fatalf("both tokens exist until the sweep: %d", n)
	}
	sw, err := e.svc.Sweep(bg, userID)
	if err != nil || sw.Deleted != 1 || !sw.Clean || sw.Unknown != 0 {
		t.Fatalf("sweep = %+v %v", sw, err)
	}
	if labels := e.remoteLabels(); len(labels) != 1 || labels[0] == oldLabel {
		t.Fatalf("remote = %v", labels)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("a recorded label is never an anomaly: %+v", p)
	}
}

func TestRenewHandlesADeadCredential(t *testing.T) {
	// revoked in the web UI
	e := enrolled(t)
	e.deleteRemote(t, e.token(t, userID).Label)
	_, err := e.svc.Renew(bg, userID)
	if !errors.Is(err, ErrCredentialInvalid) || !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if e.token(t, userID).Status != store.TokenInvalid {
		t.Fatal("the token must be marked invalid")
	}
	if p := e.policy(t, userID); p.PauseReason != store.PauseCredentialInvalid {
		t.Fatalf("policy = %+v", p)
	}
	// the attempted label is closed: the create was refused, nothing exists
	for _, l := range e.labels(t, userID) {
		if l.Label != e.token(t, userID).Label && l.DeletedAt == nil {
			t.Fatalf("open label after a refused create: %+v", l)
		}
	}

	// the sidecar was down past the expiry
	e2 := enrolled(t)
	e2.fake.Advance(80 * time.Hour)
	if _, err := e2.svc.Renew(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("err = %v", err)
	}
	if p := e2.policy(t, userID); p.PauseReason != store.PauseCredentialExpired {
		t.Fatalf("policy = %+v", p)
	}
}

func TestSweepFlagsUnknownTokensWithOurPrefixOnly(t *testing.T) {
	e := enrolled(t)
	bearer := ocisclient.Bearer(e.bearer)
	// the user's own app passwords and another instance are not our business
	for _, l := range []string{"my-phone", "ocis-agent-someoneelse-abc"} {
		if _, err := e.client.CreateToken(bg, bearer, time.Hour, l); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.svc.Sweep(bg, userID)
	if err != nil || res.Unknown != 0 || res.Deleted != 0 || !res.Clean {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("foreign labels must not pause the agent: %+v", p)
	}

	// a token with OUR prefix that we never recorded is an anomaly
	intruder := e.svc.LabelPrefix() + "intruder"
	if _, err := e.client.CreateToken(bg, bearer, time.Hour, intruder); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.Sweep(bg, userID)
	if err != nil || res.Unknown != 1 {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if p := e.policy(t, userID); !p.Paused || p.PauseReason != store.PauseTokenAnomaly {
		t.Fatalf("policy = %+v", p)
	}
	evs := e.events(t, userID)
	last := evs[len(evs)-1]
	if last.Kind != "token_anomaly" || last.Detail != "unknown=1" {
		t.Fatalf("last audit = %+v", last)
	}
	e.noLeaks(t, userID, "intruder", "my-phone", "someoneelse")
	// the monitor reports; it does not delete what it cannot attribute
	found := false
	for _, l := range e.remoteLabels() {
		found = found || l == intruder
	}
	if !found {
		t.Fatal("the unknown token must not be deleted automatically")
	}
	// a repeat finds the same thing and must not spam the audit log
	before := len(e.events(t, userID))
	if _, err := e.svc.Sweep(bg, userID); err != nil {
		t.Fatal(err)
	}
	if got := len(e.events(t, userID)); got != before {
		t.Fatalf("a repeated anomaly added %d audit event(s)", got-before)
	}
}

func TestSweepOnADeadCredentialMarksItInvalid(t *testing.T) {
	e := enrolled(t)
	e.deleteRemote(t, e.token(t, userID).Label)
	if _, err := e.svc.Sweep(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("err = %v", err)
	}
	if e.token(t, userID).Status != store.TokenInvalid {
		t.Fatal("the token must be marked invalid")
	}
}

func TestSweepNeverTouchesAnotherUsersTokens(t *testing.T) {
	e := enrolled(t)
	e.fake.AddUser("marie", "pw", "7a7a", "drive2")
	marie := Identity{UserID: "7a7a", Username: "marie", DriveID: "drive2"}
	mLabel := e.seed(t, marie, time.Hour)
	// an orphan of einstein's: recorded for einstein, created for einstein
	orphan := e.svc.newLabel()
	_ = e.st.InTx(bg, func(tx store.Tx) error { return tx.AddTokenLabel(userID, orphan) })
	cr, _ := e.svc.AppCreds(bg, userID)
	if _, err := e.client.CreateToken(bg, cr, time.Hour, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Sweep(bg, userID); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tk := range e.fake.Tokens() {
		if tk.User == "marie" && tk.Label == mLabel {
			found = true
		}
		if tk.Label == orphan {
			t.Fatal("einstein's orphan must be gone")
		}
	}
	if !found {
		t.Fatal("marie's token must be untouched")
	}
}

// our own labels legitimately appear in the audit log (token_created), so the
// detail check is exact instead of a leak scan
func (e env) anomalyCheck(t *testing.T, res SweepResult) {
	t.Helper()
	if res.Unknown != 1 {
		t.Fatalf("sweep = %+v", res)
	}
	if p := e.policy(t, userID); !p.Paused || p.PauseReason != store.PauseTokenAnomaly {
		t.Fatalf("policy = %+v", p)
	}
	evs := e.events(t, userID)
	if last := evs[len(evs)-1]; last.Kind != "token_anomaly" || last.Detail != "unknown=1" {
		t.Fatalf("last audit = %+v", last)
	}
}

func TestSweepFlagsATokenUnderAClosedLabel(t *testing.T) {
	e := enrolled(t)
	oldLabel := e.token(t, userID).Label
	e.fake.Advance(40 * time.Hour)
	if r, err := e.svc.Renew(bg, userID); err != nil || !r.OldCleaned {
		t.Fatalf("renew = %+v %v", r, err)
	}
	if res, err := e.svc.Sweep(bg, userID); err != nil || res.Unknown != 0 || !res.Clean {
		t.Fatalf("a single legitimate token must be clean: %+v %v", res, err)
	}
	if _, err := e.client.CreateToken(bg, ocisclient.Bearer(e.bearer), time.Hour, oldLabel); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Sweep(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	e.anomalyCheck(t, res)
	if res.Deleted != 0 {
		t.Fatalf("unattributable tokens are not deleted: %+v", res)
	}
	found := false
	for _, l := range e.remoteLabels() {
		found = found || l == oldLabel
	}
	if !found {
		t.Fatal("the token under the closed label must not be deleted")
	}
}

func TestSweepFlagsASecondTokenUnderTheCurrentLabel(t *testing.T) {
	e := enrolled(t)
	cur := e.token(t, userID).Label
	if res, err := e.svc.Sweep(bg, userID); err != nil || res.Unknown != 0 {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if _, err := e.client.CreateToken(bg, ocisclient.Bearer(e.bearer), time.Hour, cur); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Sweep(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	e.anomalyCheck(t, res)
	if n := len(e.fake.Tokens()); n != 2 {
		t.Fatalf("nothing may be deleted: %d tokens", n)
	}
}

func TestSweepKeepsYoungAbsentLabelsOpenUntilTheGraceEnds(t *testing.T) {
	e := enrolled(t)
	young := e.svc.newLabel()
	if err := e.st.InTx(bg, func(tx store.Tx) error { return tx.AddTokenLabel(userID, young) }); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Sweep(bg, userID)
	if err != nil || res.Closed != 0 || res.Deleted != 0 || res.Clean {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	e.fake.Advance(6 * time.Minute)
	res, err = e.svc.Sweep(bg, userID)
	if err != nil || res.Closed != 1 || res.Deleted != 0 || !res.Clean {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if k := kinds(e.events(t, userID)); !strings.HasSuffix(k, "token_label_closed") {
		t.Fatalf("audit = %s", k)
	}
}

func TestALateLandingTokenUnderAYoungLabelIsDeletedNotFlagged(t *testing.T) {
	e := enrolled(t)
	young := e.svc.newLabel()
	if err := e.st.InTx(bg, func(tx store.Tx) error { return tx.AddTokenLabel(userID, young) }); err != nil {
		t.Fatal(err)
	}
	if res, err := e.svc.Sweep(bg, userID); err != nil || res.Clean {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	cr, _ := e.svc.AppCreds(bg, userID)
	if _, err := e.client.CreateToken(bg, cr, time.Hour, young); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Sweep(bg, userID)
	if err != nil || res.Deleted != 1 || res.Unknown != 0 || !res.Clean {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("our own late token is not an anomaly: %+v", p)
	}
	if labels := e.remoteLabels(); len(labels) != 1 {
		t.Fatalf("remote = %v", labels)
	}
}
