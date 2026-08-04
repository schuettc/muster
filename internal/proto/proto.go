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
	IdemKey string `json:"idem,omitempty"`
}

// Response is the daemon's reply. Exactly one of Data/Error is meaningful.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
