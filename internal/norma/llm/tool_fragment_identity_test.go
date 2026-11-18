// Modified by ARTEX for in-tree integration; see ../NOTICE.
package llm

import "testing"

type expectedToolUse struct {
	id        string
	name      string
	input     string
	signature string
}

func requireToolUses(t *testing.T, acc *Accumulator, want []expectedToolUse) {
	t.Helper()
	got := acc.Message().ToolUses()
	if len(got) != len(want) {
		t.Fatalf("got %d tool uses, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].id || got[i].Name != want[i].name ||
			string(got[i].Input) != want[i].input || got[i].ThoughtSignature != want[i].signature {
			t.Errorf("tool use %d = {ID:%q Name:%q Input:%s ThoughtSignature:%q}, want {ID:%q Name:%q Input:%s ThoughtSignature:%q}",
				i, got[i].ID, got[i].Name, got[i].Input, got[i].ThoughtSignature,
				want[i].id, want[i].name, want[i].input, want[i].signature)
		}
	}
}

func TestAccumulatorRoutesTwoInterleavedToolInputsByID(t *testing.T) {
	acc := NewAccumulator()
	for _, ev := range []StreamEvent{
		{Type: SEToolUseStart, ToolID: "call-0", ToolName: "Read", ToolIndex: 0},
		{Type: SEToolUseStart, ToolID: "call-1", ToolName: "Search", ToolIndex: 1},
		{Type: SEToolInputJSON, ToolID: "call-0", Text: `{"path":"`},
		{Type: SEToolInputJSON, ToolID: "call-1", Text: `{"query":"`},
		{Type: SEToolInputJSON, ToolID: "call-0", Text: `alpha"}`},
		{Type: SEToolInputJSON, ToolID: "call-1", Text: `beta"}`},
		{Type: SEToolUseSignature, ToolIndex: 1, Text: "sig-1"},
		{Type: SEToolUseSignature, ToolIndex: 0, Text: "sig-0"},
	} {
		acc.Add(ev)
	}

	requireToolUses(t, acc, []expectedToolUse{
		{id: "call-0", name: "Read", input: `{"path":"alpha"}`, signature: "sig-0"},
		{id: "call-1", name: "Search", input: `{"query":"beta"}`, signature: "sig-1"},
	})
}

func TestAccumulatorRoutesThreeInterleavedToolInputsAndIndexesNewStarts(t *testing.T) {
	acc := NewAccumulator()
	for _, ev := range []StreamEvent{
		{Type: SEToolUseStart, ToolID: "call-0", ToolName: "First", ToolIndex: 0},
		// The first explicit fragment lazily creates the ID index. Later starts
		// must be added to that existing index.
		{Type: SEToolInputJSON, ToolID: "call-0", Text: `{"value":`},
		{Type: SEToolUseStart, ToolID: "call-1", ToolName: "Second", ToolIndex: 1},
		{Type: SEToolInputJSON, ToolID: "call-1", Text: `{"value":`},
		{Type: SEToolUseStart, ToolID: "call-2", ToolName: "Third", ToolIndex: 2},
		{Type: SEToolInputJSON, ToolID: "call-2", Text: `{"value":`},
		{Type: SEToolInputJSON, ToolID: "call-2", Text: `3}`},
		{Type: SEToolInputJSON, ToolID: "call-0", Text: `1}`},
		{Type: SEToolInputJSON, ToolID: "call-1", Text: `2}`},
		{Type: SEToolUseSignature, ToolIndex: 2, Text: "sig-2"},
		{Type: SEToolUseSignature, ToolIndex: 0, Text: "sig-0"},
		{Type: SEToolUseSignature, ToolIndex: 1, Text: "sig-1"},
	} {
		acc.Add(ev)
	}

	requireToolUses(t, acc, []expectedToolUse{
		{id: "call-0", name: "First", input: `{"value":1}`, signature: "sig-0"},
		{id: "call-1", name: "Second", input: `{"value":2}`, signature: "sig-1"},
		{id: "call-2", name: "Third", input: `{"value":3}`, signature: "sig-2"},
	})
}

func TestAccumulatorEmptyToolInputIDKeepsLatestCallSemantics(t *testing.T) {
	acc := NewAccumulator()
	for _, ev := range []StreamEvent{
		{Type: SEToolUseStart, ToolID: "call-0", ToolName: "First", ToolIndex: 0},
		{Type: SEToolInputJSON, ToolIndex: 7, Text: `{"value":"first"}`},
		{Type: SEToolUseStart, ToolID: "call-1", ToolName: "Second", ToolIndex: 1},
		// ToolIndex deliberately names call-0. Empty ToolID must still target
		// the most recently started call, as legacy OpenAI deltas always did.
		{Type: SEToolInputJSON, ToolIndex: 0, Text: `{"value":"second"}`},
	} {
		acc.Add(ev)
	}
	if acc.toolIDPos != nil {
		t.Fatal("legacy fragments allocated the explicit-ID routing index")
	}

	requireToolUses(t, acc, []expectedToolUse{
		{id: "call-0", name: "First", input: `{"value":"first"}`},
		{id: "call-1", name: "Second", input: `{"value":"second"}`},
	})
}

func TestAccumulatorUnknownToolInputIDDoesNotCorruptLatestCall(t *testing.T) {
	acc := NewAccumulator()
	for _, ev := range []StreamEvent{
		{Type: SEToolUseStart, ToolID: "call-0", ToolName: "First"},
		{Type: SEToolUseStart, ToolID: "call-1", ToolName: "Second"},
		{Type: SEToolInputJSON, ToolID: "call-0", Text: `{"value":`},
		{Type: SEToolInputJSON, ToolID: "call-1", Text: `{"value":`},
		{Type: SEToolInputJSON, ToolID: "unknown", Text: `"corruption"`},
		{Type: SEToolInputJSON, ToolID: "call-1", Text: `2}`},
		{Type: SEToolInputJSON, ToolID: "call-0", Text: `1}`},
	} {
		acc.Add(ev)
	}

	requireToolUses(t, acc, []expectedToolUse{
		{id: "call-0", name: "First", input: `{"value":1}`},
		{id: "call-1", name: "Second", input: `{"value":2}`},
	})
}

func TestAccumulatorResetClearsToolInputIDIndex(t *testing.T) {
	acc := NewAccumulator()
	acc.Add(StreamEvent{Type: SEToolUseStart, ToolID: "old", ToolName: "Old"})
	acc.Add(StreamEvent{Type: SEToolInputJSON, ToolID: "old", Text: `{}`})
	if acc.toolIDPos == nil {
		t.Fatal("explicit-ID fragment did not create routing index")
	}

	acc.Reset()
	if acc.toolIDPos != nil {
		t.Fatal("Reset retained tool-input ID routing index")
	}
	acc.Add(StreamEvent{Type: SEToolUseStart, ToolID: "new", ToolName: "New"})
	acc.Add(StreamEvent{Type: SEToolInputJSON, ToolID: "old", Text: `{"stale":true}`})
	acc.Add(StreamEvent{Type: SEToolInputJSON, ToolID: "new", Text: `{"fresh":true}`})

	requireToolUses(t, acc, []expectedToolUse{
		{id: "new", name: "New", input: `{"fresh":true}`},
	})
}
