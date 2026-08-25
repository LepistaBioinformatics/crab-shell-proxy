package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The proxy has no opinion about file types (quick/002). These are the names
// members actually brought and could not upload: a format no allowlist would have
// guessed, and a file with no extension at all — `filepath.Ext` answers "" for it,
// so it failed the old membership check without ever being a type anyone refused.
//
// The workaround the refusal produced was worse than the refusal: renaming a file
// so the picker would show it leaves an extension that LIES about the bytes, and
// the agent then opens it as the type the name claims.
func TestMediaUploadAcceptsAnyFilename(t *testing.T) {
	for _, name := range []string{
		"sample.parquet",
		"reads.fastq.gz",
		"scan.dcm",
		"Makefile",
		"LICENSE",
		"notes.TXT",
	} {
		t.Run(name, func(t *testing.T) {
			orch := scaffoldedOrch()
			s := uploadServer(orch)

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, mediaUploadReqNamed(t, "", name))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

// The size cap is a separate rule with a separate reason, and dropping the type
// gate must not have loosened it.
//
// The bound is `MediaMaxBytes + 1 MiB` (handlers.go), so the payload has to clear
// that slack for the refusal to be the cap's rather than the reader's arithmetic.
// The slack is deliberate — it covers the multipart envelope — but it does mean
// the enforced ceiling is a megabyte above the configured one, which is why the
// per-scope cap in admin-managed-storage-limits is checked against `header.Size`
// after the parse rather than by tightening this reader.
func TestMediaUploadStillHonoursTheSizeCap(t *testing.T) {
	orch := scaffoldedOrch()
	s := uploadServer(orch)
	s.Cfg.MediaMaxBytes = 1024

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, mediaUploadReqSized(t, "sample.parquet", 2<<20))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", w.Code, w.Body.String())
	}
}
