package storetest

// StoredResults are approval results (Approval.Result) of every shape the
// decoders must agree on: the executor's, and unreadable or unexpected
// ones. Tests of every reader of stored outcomes run over them, so the store
// (needs_verification), the API (outcome) and the executor (reconcile)
// classify each the same way.
var StoredResults = []string{
	`{"outcome":"done"}`,
	`{"outcome":"done","result":{"summary":"ok","diff":"+a"}}`,
	`{"outcome":"done","result":{"summary":"ok","blocks":[{"source":"file","path":"/a","sha256":"x","content":"c"}]}}`,
	`{"outcome":"done","result":null}`,
	`{"outcome":"failed","reason":"no"}`,
	`{"outcome":"expired","reason":"late"}`,
	`{"outcome":"unknown","reason":"verify"}`,
	`{"outcome":"maybe"}`,
	`{"outcome":""}`,
	`{}`,
	`null`,
	`["strange"]`,
	`"text"`,
	`{"outcome":5}`,
	`{"outcome":"done","result":"x"}`,
	`{"outcome":"done","result":{"summary":5}}`,
	`{"outcome":"done","result":{"diff":[]}}`,
	`{"outcome":"done","result":{"blocks":5}}`,
	`{"outcome":"done","result":{"blocks":[{"content":1}]}}`,
	`{"outcome":"done","reason":7}`,
	`{"Outcome":"unknown","outcome":"done"}`,
	`{broken`,
}
