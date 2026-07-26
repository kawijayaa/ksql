package features

import (
	"encoding/json"
	"testing"
)

func TestLedgerInventory(t *testing.T) {
	entries := All()
	// The pinned registries contain 537 function entries in addition to
	// operators, sources, statements, and scalar operators.
	if len(entries) < 650 {
		t.Fatalf("ledger has %d entries, want at least 650", len(entries))
	}
	for _, id := range []string{
		"operator.graph-shortest-paths",
		"function.geo_h3cell_children",
		"aggregate.arg_max",
		"plugin.ai_chat_completion",
	} {
		if _, ok := Lookup(id); !ok {
			t.Errorf("missing %s", id)
		}
	}
}

func TestLedgerJSON(t *testing.T) {
	var entries []Entry
	if err := json.Unmarshal(JSON(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(All()) {
		t.Fatalf("JSON entries = %d, All = %d", len(entries), len(All()))
	}
}
