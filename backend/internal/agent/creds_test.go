package agent

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// basicSecrets returns the Basic-auth secret of every request with this
// method, in order.
func (f *fixture) basicSecrets(method string) []string {
	var out []string
	for _, r := range f.fake.Requests() {
		if r.Method != method {
			continue
		}
		_, pw, _ := (&http.Request{Header: r.Header}).BasicAuth()
		out = append(out, pw)
	}
	return out
}

func TestARenewedTokenIsUsedWithinTheSameClaim(t *testing.T) {
	f := newFixture(t)
	tok, err := f.client.CreateToken(bg, ocisclient.AppToken(login, password), time.Hour, "renewed")
	if err != nil {
		t.Fatal(err)
	}
	turn := 0
	f.provider = llm.ProviderFunc(func(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
		turn++
		switch turn {
		case 1:
			return llmtest.Calls(1, 1, list("a", "/")).Resp, nil
		case 2:
			f.creds.setSecret(tok.Value.Reveal()) // the token was renewed meanwhile
			return llmtest.Calls(1, 1, list("b", "/x")).Resp, nil
		}
		return llmtest.Say("done", 1, 1).Resp, nil
	})
	f.enqueue("g")
	f.fake.ResetRequests()
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	// the excerpt of the last turn reads memory with the new token too
	if gets := f.basicSecrets("GET"); len(gets) == 0 || gets[len(gets)-1] != tok.Value.Reveal() {
		t.Errorf("the last memory read did not use the renewed token")
	}
	got := f.basicSecrets("PROPFIND")
	if len(got) < 2 || got[0] != password || got[len(got)-1] != tok.Value.Reveal() {
		t.Fatalf("PROPFIND secrets = %d, first old %v, last new %v", len(got), len(got) > 0 && got[0] == password,
			len(got) > 0 && got[len(got)-1] == tok.Value.Reveal())
	}
}
