// Package audiovalidate provides lightweight audio-file integrity checks that
// run before expensive transcription or disk-write operations.
//
// The checks are intentionally minimal: they guard against obviously-corrupt
// files (e.g. 4096-byte all-zero uploads) rather than full format validation.
// False-negatives (corrupt files that pass) are handled downstream; these
// guards exist to short-circuit the infinite-retry loop caused by files that
// will never successfully decode.
package audiovalidate

import "errors"

// minAudioBytes is the smallest byte count that could plausibly be a valid
// audio container. Any file shorter than this is almost certainly garbage.
// 8 bytes covers the minimum ISOBMFF/MP4 box header (4-byte size + 4-byte type).
const minAudioBytes = 8

// ErrTooShort is returned when the provided data is shorter than minAudioBytes.
var ErrTooShort = errors.New("audio file too short to be a valid container")

// ErrNotM4A is returned when the data does not carry the ISO Base Media File
// Format "ftyp" box at offset 4, which is required for m4a/mp4 audio.
var ErrNotM4A = errors.New("audio file missing ftyp box at offset 4 (not a valid m4a/mp4)")

// CheckM4A validates the leading bytes of an m4a/mp4 audio payload.
//
// Checks performed (in order):
//  1. Minimum length: len(data) >= 8. Shorter data is rejected with ErrTooShort.
//  2. ftyp magic: bytes[4:8] == "ftyp". Files that lack this box are rejected
//     with ErrNotM4A.
//
// All other file formats (mp3, wav, etc.) should use CheckAudioBytes instead,
// which only enforces the minimum-length guard.
//
// Returns nil when the data passes both checks.
func CheckM4A(data []byte) error {
	if len(data) < minAudioBytes {
		return ErrTooShort
	}
	// ISO Base Media File Format (ISOBMFF / MP4 / M4A):
	// The first box starts at offset 0:
	//   bytes [0:4] — 32-bit big-endian box size (not checked; may be 0 for "to EOF")
	//   bytes [4:8] — 4-byte box type, which MUST be "ftyp" for valid m4a/mp4 files
	if string(data[4:8]) != "ftyp" {
		return ErrNotM4A
	}
	return nil
}

// CheckAudioBytes enforces the minimum-length guard only, without any
// container-format check. Use this for non-m4a audio formats (mp3, wav, etc.)
// where byte[4:8] does not carry a known magic value.
//
// Returns ErrTooShort when len(data) < 8, nil otherwise.
func CheckAudioBytes(data []byte) error {
	if len(data) < minAudioBytes {
		return ErrTooShort
	}
	return nil
}
