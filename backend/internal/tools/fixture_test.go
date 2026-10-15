package tools

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

const testDrive = "drive1"

type fixture struct {
	env  Env
	fake *fakeocis.Server
	st   store.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	fake := fakeocis.New(t)
	fake.AddUser("einstein", "pw", "u1", testDrive)
	client, err := ocisclient.New(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InTx(bg, func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: "u1", Name: "einstein", PersonalDriveID: testDrive})
	}); err != nil {
		t.Fatal(err)
	}
	return fixture{
		env: Env{
			UserID: "u1", DriveID: testDrive, Creds: ocisclient.AppToken("einstein", "pw"),
			Files: client, Memory: memory.New(client, st, memory.Limits{}),
		},
		fake: fake,
		st:   st,
	}
}

// put writes a file out of band (parents are created) and returns its ETag.
func (f fixture) put(path, content string) string {
	return f.fake.PutFile(testDrive, path, []byte(content))
}

// run prepares and executes a tool like the gate and the executor will:
// Prepare first, then Execute with the prepared target ETag.
func (f fixture) run(t *testing.T, tool Tool, args string) (Result, error) {
	t.Helper()
	raw := json.RawMessage(args)
	p, err := tool.Prepare(bg, f.env, raw)
	if err != nil {
		return Result{}, err
	}
	return tool.Execute(bg, f.env, raw, ExecInfo{IdemKey: "k", TargetETag: p.TargetETag})
}

// requestMethods lists the HTTP methods the fake saw, one per request.
func requestMethods(fake *fakeocis.Server) []string {
	var m []string
	for _, r := range fake.Requests() {
		m = append(m, r.Method)
	}
	return m
}
