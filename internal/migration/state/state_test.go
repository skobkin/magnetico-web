package state

import (
	"errors"
	"regexp"
	"testing"
)

func TestPhaseHelpers(t *testing.T) {
	t.Parallel()

	for _, p := range []Phase{PhaseImporting, PhaseDataLoaded, PhaseBuildingIndexes,
		PhaseImportComplete, PhaseCompleteWithErrors} {
		if !p.Valid() {
			t.Fatalf("phase %q must be valid", p)
		}
	}
	if Phase("nonsense").Valid() {
		t.Fatal("unknown phase must be invalid")
	}
	if !PhaseImportComplete.Complete() || !PhaseCompleteWithErrors.Complete() {
		t.Fatal("complete phases must report Complete")
	}
	for _, p := range []Phase{PhaseImporting, PhaseDataLoaded, PhaseBuildingIndexes} {
		if p.Complete() {
			t.Fatalf("phase %q must not report Complete", p)
		}
	}
}

func TestValidateAdvance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      AdvanceInput
		wantErr bool
	}{
		{
			name: "importing advances within phase",
			in:   AdvanceInput{ExpectedPhase: PhaseImporting, NewPhase: PhaseImporting, ExpectedLastID: 10, NewLastID: 20},
		},
		{
			name: "importing to data_loaded",
			in:   AdvanceInput{ExpectedPhase: PhaseImporting, NewPhase: PhaseDataLoaded, ExpectedLastID: 10, NewLastID: 10},
		},
		{
			name: "data_loaded to building_indexes",
			in:   AdvanceInput{ExpectedPhase: PhaseDataLoaded, NewPhase: PhaseBuildingIndexes, ExpectedLastID: 10, NewLastID: 10},
		},
		{
			name: "building_indexes advances within phase",
			in:   AdvanceInput{ExpectedPhase: PhaseBuildingIndexes, NewPhase: PhaseBuildingIndexes, ExpectedLastID: 10, NewLastID: 10},
		},
		{
			name: "building_indexes completes",
			in:   AdvanceInput{ExpectedPhase: PhaseBuildingIndexes, NewPhase: PhaseImportComplete, ExpectedLastID: 10, NewLastID: 10},
		},
		{
			name:    "checkpoint moves backwards",
			in:      AdvanceInput{ExpectedPhase: PhaseImporting, NewPhase: PhaseImporting, ExpectedLastID: 20, NewLastID: 10},
			wantErr: true,
		},
		{
			name:    "importing jumps to building_indexes",
			in:      AdvanceInput{ExpectedPhase: PhaseImporting, NewPhase: PhaseBuildingIndexes, ExpectedLastID: 10, NewLastID: 10},
			wantErr: true,
		},
		{
			name:    "data_loaded jumps to complete",
			in:      AdvanceInput{ExpectedPhase: PhaseDataLoaded, NewPhase: PhaseImportComplete, ExpectedLastID: 10, NewLastID: 10},
			wantErr: true,
		},
		{
			name:    "completed run cannot advance",
			in:      AdvanceInput{ExpectedPhase: PhaseImportComplete, NewPhase: PhaseBuildingIndexes, ExpectedLastID: 10, NewLastID: 10},
			wantErr: true,
		},
		{
			name:    "unknown phase",
			in:      AdvanceInput{ExpectedPhase: Phase("nonsense"), NewPhase: PhaseImporting, ExpectedLastID: 1, NewLastID: 2},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateAdvance(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAdvance() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrRunStateCorrupt) {
				t.Fatalf("validateAdvance() error = %v, want ErrRunStateCorrupt", err)
			}
		})
	}
}

func TestNewRunID(t *testing.T) {
	t.Parallel()

	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID() error = %v", err)
		}
		if !pattern.MatchString(id) {
			t.Fatalf("NewRunID() = %q, not a v4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("NewRunID() repeated %q", id)
		}
		seen[id] = true
	}
}
