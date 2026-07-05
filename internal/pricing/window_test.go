package pricing

import "testing"

func TestWindowFromDataset(t *testing.T) {
	// Seed a fetched dataset the way parseInto would.
	parseInto([]byte(`{
	  "gpt-4o": {"input_cost_per_token": 0.0000025, "output_cost_per_token": 0.00001, "max_input_tokens": 128000},
	  "claude-sonnet-4-6": {"input_cost_per_token": 0.000003, "output_cost_per_token": 0.000015, "max_tokens": 200000},
	  "some-model": {"input_cost_per_token": 0.000001, "max_input_tokens": 32000}
	}`))

	cases := []struct {
		model string
		want  int
		ok    bool
	}{
		{"gpt-4o", 128000, true},                 // max_input_tokens
		{"claude-sonnet-4-6", 200000, true},      // falls back to max_tokens
		{"claude-sonnet-4-6:high", 200000, true}, // thinking suffix stripped
		{"some-model", 32000, true},              // matched after the "/" prefix
		{"never-heard-of-it", 0, false},          // unknown → local override territory
	}
	for _, c := range cases {
		got, ok := Window(c.model)
		if got != c.want || ok != c.ok {
			t.Errorf("Window(%q) = (%d,%v), want (%d,%v)", c.model, got, ok, c.want, c.ok)
		}
	}
}
