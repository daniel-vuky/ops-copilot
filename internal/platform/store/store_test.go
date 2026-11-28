package store

import (
	"context"
	"testing"
)

// fakeTx is a TxState whose Done result is fixed.
type fakeTx struct{ done bool }

func (f *fakeTx) Done() bool { return f.done }

func TestInWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"no state", context.Background(), false},
		{"live transaction", WithTx(context.Background(), &fakeTx{done: false}), true},
		{"ended transaction", WithTx(context.Background(), &fakeTx{done: true}), false},
		{"nil state", WithTx(context.Background(), nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := InWrite(tt.ctx); got != tt.want {
				t.Errorf("InWrite = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTxFromRoundTrip(t *testing.T) {
	t.Parallel()

	if s, ok := TxFrom(context.Background()); ok || s != nil {
		t.Errorf("TxFrom(empty) = %v, %v; want nil, false", s, ok)
	}

	want := &fakeTx{}
	ctx := WithTx(context.Background(), want)
	got, ok := TxFrom(ctx)
	if !ok {
		t.Fatal("TxFrom: ok = false, want true")
	}
	if got != want {
		t.Errorf("TxFrom = %p, want %p", got, want)
	}

	// A derived context still carries the state.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if got, ok := TxFrom(child); !ok || got != want {
		t.Errorf("TxFrom(child) = %v, %v; want %p, true", got, ok, want)
	}
}
