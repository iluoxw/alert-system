package domain

import "testing"

func TestImpactStateIsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state ImpactState
		want  bool
	}{
		{name: "potential", state: ImpactStatePotential, want: true},
		{name: "confirmed", state: ImpactStateConfirmed, want: true},
		{name: "ruled out", state: ImpactStateRuledOut, want: true},
		{name: "unknown", state: ImpactState("likely"), want: false},
		{name: "empty", state: ImpactState(""), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.state.IsValid(); got != test.want {
				t.Fatalf("ImpactState(%q).IsValid() = %v, want %v", test.state, got, test.want)
			}
		})
	}
}
