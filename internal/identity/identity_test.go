package identity

import (
	"encoding/base64"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// encodeProfile builds a header value the same way mycelium does:
// base64(zstd(json)). Used to drive the decoder in tests.
func encodeProfile(t *testing.T, jsonBody string) string {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := enc.EncodeAll([]byte(jsonBody), nil)
	_ = enc.Close()
	return base64.StdEncoding.EncodeToString(compressed)
}

func TestResolve(t *testing.T) {
	r, err := NewFallbackResolver()
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	tests := []struct {
		name      string
		header    string
		wantOK    bool
		wantAcc   string
		wantEmail string
	}{
		{
			name:      "accId + principal email",
			header:    encodeProfile(t, `{"accId":"acc-123","owners":[{"email":"other@x","isPrincipal":false},{"email":"alice@x","isPrincipal":true}]}`),
			wantOK:    true,
			wantAcc:   "acc-123",
			wantEmail: "alice@x",
		},
		{
			name:      "accId with no owners still resolves (email empty)",
			header:    encodeProfile(t, `{"accId":"acc-xyz","owners":[]}`),
			wantOK:    true,
			wantAcc:   "acc-xyz",
			wantEmail: "",
		},
		{
			name:   "missing accId => not resolvable",
			header: encodeProfile(t, `{"owners":[{"email":"a@x","isPrincipal":true}]}`),
			wantOK: false,
		},
		{name: "empty header", header: "", wantOK: false},
		{name: "not base64", header: "!!!", wantOK: false},
		{name: "base64 but not zstd", header: base64.StdEncoding.EncodeToString([]byte("plain")), wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := r.Resolve(tt.header)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if id.AccID != tt.wantAcc {
					t.Errorf("AccID = %q, want %q", id.AccID, tt.wantAcc)
				}
				if id.Email != tt.wantEmail {
					t.Errorf("Email = %q, want %q", id.Email, tt.wantEmail)
				}
			}
		})
	}
}

func TestSanitizeID(t *testing.T) {
	// A real UUID passes through unchanged (already Docker-safe).
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	if got := SanitizeID(uuid); got != uuid {
		t.Errorf("SanitizeID(uuid) = %q, want unchanged", got)
	}
	// Unexpected chars are replaced; result is Docker-name-safe.
	got := SanitizeID("acc id/with:weird*chars")
	for _, c := range got {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if !ok {
			t.Errorf("SanitizeID left unsafe char %q in %q", c, got)
		}
	}
	if SanitizeID("") == "" {
		t.Error("SanitizeID must never return empty")
	}
}

func TestSessionKey(t *testing.T) {
	a := SessionKey("acc-1", "conv-1")
	if len(a) != 32 {
		t.Errorf("len = %d, want 32", len(a))
	}
	if SessionKey("acc-1", "conv-1") != a {
		t.Error("must be deterministic")
	}
	if SessionKey("acc-2", "conv-1") == a {
		t.Error("different accId must differ")
	}
	if SessionKey("acc-1", "conv-2") == a {
		t.Error("different session must differ")
	}
	if SessionKey("", "x") != "" || SessionKey("x", "") != "" {
		t.Error("empty input => empty key")
	}
}
