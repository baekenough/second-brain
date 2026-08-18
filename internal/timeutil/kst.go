// Package timeutil holds the single timezone Second Brain resolves calendar
// boundaries and renders dates in.
//
// It exists because the process's own local timezone is NOT a safe default:
// the server and collector containers run as Etc/UTC, while the developer
// machines, the users, and every ingested document are Korea-based. Code that
// reaches for time.Now().Location() therefore behaves one way in tests and a
// different way in production — silently, and off by nine hours. Anything that
// answers "which calendar day/week/month is this?" or renders a date for a
// human (or for an LLM prompt) must go through KST rather than the process
// local zone.
package timeutil

import "time"

// kstOffsetSeconds is UTC+09:00, the fixed-fallback offset for Asia/Seoul.
const kstOffsetSeconds = 9 * 60 * 60

// kst is resolved once at package initialisation: the location never changes
// during a process's lifetime, and *time.Location values are safe for
// concurrent use.
var kst = loadKST()

// KST returns the Asia/Seoul location. It never returns nil, so callers can use
// it directly in time.Date / Time.In without a nil check.
//
// time.LoadLocation reads the OS tzdata database; the production image
// (Dockerfile's runtime-base stage) installs the tzdata package for exactly
// this reason. The time.FixedZone fallback only matters for an environment
// without a tzdata database (e.g. a minimal test sandbox, or a scratch image) —
// Korea has observed no DST since 1988, so a fixed UTC+9 offset is equivalent
// to the IANA zone at every point in time this system deals with, not merely an
// approximation. Falling back rather than panicking keeps a missing tzdata
// package from taking the whole service down.
func KST() *time.Location {
	return kst
}

func loadKST() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return time.FixedZone("KST", kstOffsetSeconds)
	}
	return loc
}
