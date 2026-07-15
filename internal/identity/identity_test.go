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

func TestPrincipalEmail(t *testing.T) {
	r, err := NewFallbackResolver()
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "principal owner chosen over others",
			header: encodeProfile(t, `{"owners":[{"email":"other@x","isPrincipal":false},{"email":"alice@x","isPrincipal":true}]}`),
			want:   "alice@x",
		},
		{
			name:   "falls back to first owner when none principal",
			header: encodeProfile(t, `{"owners":[{"email":"bob@x","isPrincipal":false}]}`),
			want:   "bob@x",
		},
		{
			name:   "empty header",
			header: "",
			want:   "",
		},
		{
			name:   "not base64",
			header: "!!!not base64!!!",
			want:   "",
		},
		{
			name:   "base64 but not zstd",
			header: base64.StdEncoding.EncodeToString([]byte("plain text")),
			want:   "",
		},
		{
			name:   "no owners",
			header: encodeProfile(t, `{"owners":[]}`),
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.PrincipalEmail(tt.header); got != tt.want {
				t.Errorf("PrincipalEmail() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUserHash(t *testing.T) {
	a := UserHash("Alice@X")
	b := UserHash("alice@x ")
	if a != b {
		t.Errorf("UserHash should be case/space insensitive: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("UserHash length = %d, want 16", len(a))
	}
	for _, c := range a {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Errorf("UserHash contains non-hex (Docker-unsafe) char %q", c)
		}
	}
	if UserHash("alice@x") == UserHash("bob@x") {
		t.Error("distinct emails must hash distinctly")
	}
}
