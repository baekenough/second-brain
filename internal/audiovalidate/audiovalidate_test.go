package audiovalidate_test

import (
	"errors"
	"testing"

	"github.com/baekenough/second-brain/internal/audiovalidate"
)

func TestCheckM4A(t *testing.T) {
	t.Parallel()

	// validHeader is a minimal 8-byte buffer with "ftyp" at offset 4.
	validHeader := []byte{0, 0, 0, 28, 'f', 't', 'y', 'p'}

	// garageGarbage simulates the 4096-byte all-zero payload observed in production.
	garbageHeader := make([]byte, 4096) // all zeros — no ftyp box

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name:    "valid m4a header passes",
			data:    validHeader,
			wantErr: nil,
		},
		{
			name:    "nil data is too short",
			data:    nil,
			wantErr: audiovalidate.ErrTooShort,
		},
		{
			name:    "empty data is too short",
			data:    []byte{},
			wantErr: audiovalidate.ErrTooShort,
		},
		{
			name:    "7 bytes is too short",
			data:    []byte{0, 0, 0, 0, 'f', 't', 'y'},
			wantErr: audiovalidate.ErrTooShort,
		},
		{
			name:    "exactly 8 bytes without ftyp is rejected",
			data:    []byte{0, 0, 0, 0, 0, 0, 0, 0},
			wantErr: audiovalidate.ErrNotM4A,
		},
		{
			name:    "4096-byte garbage zeros is rejected",
			data:    garbageHeader,
			wantErr: audiovalidate.ErrNotM4A,
		},
		{
			name:    "RIFF header (wav) is rejected by m4a check",
			data:    []byte{'R', 'I', 'F', 'F', 'W', 'A', 'V', 'E'},
			wantErr: audiovalidate.ErrNotM4A,
		},
		{
			name: "larger buffer with ftyp at offset 4 passes",
			data: func() []byte {
				b := make([]byte, 32)
				b[4] = 'f'
				b[5] = 't'
				b[6] = 'y'
				b[7] = 'p'
				return b
			}(),
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := audiovalidate.CheckM4A(tc.data)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("CheckM4A() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckAudioBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name:    "nil is too short",
			data:    nil,
			wantErr: audiovalidate.ErrTooShort,
		},
		{
			name:    "7 bytes is too short",
			data:    make([]byte, 7),
			wantErr: audiovalidate.ErrTooShort,
		},
		{
			name:    "exactly 8 bytes passes",
			data:    make([]byte, 8),
			wantErr: nil,
		},
		{
			name:    "large buffer passes regardless of content",
			data:    make([]byte, 4096),
			wantErr: nil,
		},
		{
			name:    "RIFF wav header passes (no container check)",
			data:    []byte{'R', 'I', 'F', 'F', 'W', 'A', 'V', 'E'},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := audiovalidate.CheckAudioBytes(tc.data)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("CheckAudioBytes() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
