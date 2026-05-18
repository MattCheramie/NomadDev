package webauthn

import "crypto/rand"

// readRand is a tiny indirection on crypto/rand.Read so tests can
// swap deterministic randomness if needed. Production code goes
// through crypto/rand unconditionally.
var readRand = rand.Read
