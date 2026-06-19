package dashboard

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the process timezone to UTC for the whole package's tests.
//
// The dashboard renders timestamps with time.Time.Local() (correct for users —
// they see their own zone). Golden fixtures, however, must render identically
// on every machine. Without this, fixtures baked in one zone (e.g. the author's
// EDT) fail in CI (UTC) with an N-hour offset. Forcing time.Local = UTC makes
// golden rendering deterministic; production code is unaffected.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
