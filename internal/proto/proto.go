// Package proto defines the daemon wire protocol: newline-delimited JSON.
package proto

// Request is one operation call. Args are op-specific.
type Request struct {
	Op   string         `json:"op"`
	Args map[string]any `json:"args,omitempty"`
}

// Response is the daemon's reply. Exactly one of Data/Error is meaningful.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
