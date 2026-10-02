// Package proto defines the daemon wire protocol: newline-delimited JSON.
package proto

// Request is one operation call. Args are op-specific.
type Request struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args,omitempty"`
	// IdemKey deduplicates a write across transport retries. It is set only
	// on the daemon→upstream hop in remote mode; local dispatch ignores it,
	// and omitempty keeps it off the wire entirely for local clients, so the
	// format stays compatible with pre-remote binaries.
	//
	// A key identifies ONE REQUEST — not a caller, a session, or an op.
	// Nothing binds a recorded response to the request that produced it, so
	// reusing a key across two different requests returns the FIRST one's
	// response for the second, silently and with no error. Mint a fresh key
	// per logical write and hold it constant only across retries of that
	// same write.
	IdemKey string `json:"idem,omitempty"`
}

// Response is the daemon's reply. Exactly one of Data/Error is meaningful.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
