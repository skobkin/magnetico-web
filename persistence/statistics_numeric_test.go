package persistence

// Unit tests for the numeric → uint64 conversion of the total_size
// aggregate: Statistics.TotalSize is uint64, so values above MaxInt64 are
// realizable states, while negatives, fractions and out-of-range values
// must be refused, never truncated or wrapped.

import (
	"errors"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestNumericToUint64(t *testing.T) {
	t.Parallel()

	maxUint64 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))

	tests := []struct {
		name    string
		numeric pgtype.Numeric
		want    uint64
		wantErr bool
	}{
		{
			name:    "zero",
			numeric: pgtype.Numeric{Int: big.NewInt(0), Valid: true},
			want:    0,
		},
		{
			name:    "small value",
			numeric: pgtype.Numeric{Int: big.NewInt(42), Valid: true},
			want:    42,
		},
		{
			name:    "positive scale multiplies out",
			numeric: pgtype.Numeric{Int: big.NewInt(12), Exp: 3, Valid: true},
			want:    12000,
		},
		{
			name:    "MaxUint64 is accepted",
			numeric: pgtype.Numeric{Int: maxUint64, Valid: true},
			want:    maxUint64.Uint64(),
		},
		{
			name:    "MaxUint64 plus one is refused",
			numeric: pgtype.Numeric{Int: new(big.Int).Add(maxUint64, big.NewInt(1)), Valid: true},
			wantErr: true,
		},
		{
			name:    "negative is refused",
			numeric: pgtype.Numeric{Int: big.NewInt(-1), Valid: true},
			wantErr: true,
		},
		{
			name:    "fractional value is refused",
			numeric: pgtype.Numeric{Int: big.NewInt(15), Exp: -1, Valid: true},
			wantErr: true,
		},
		{
			name:    "NaN is refused",
			numeric: pgtype.Numeric{NaN: true, Valid: true},
			wantErr: true,
		},
		{
			name:    "NULL is refused",
			numeric: pgtype.Numeric{Valid: false},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := numericToUint64(tt.numeric)
			if tt.wantErr {
				if !errors.Is(err, ErrStorageIO) {
					t.Errorf("numericToUint64() error = %v, want ErrStorageIO", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("numericToUint64() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("numericToUint64() = %d, want %d", got, tt.want)
			}
		})
	}
}
