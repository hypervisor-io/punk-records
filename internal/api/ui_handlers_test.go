package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestBrainRoutes(t *testing.T) {
	s := testServer(t)
	s.MountBrain()
	for _, tc := range []struct{ path, ctype, want string }{
		{"/", "text/html; charset=utf-8", "<canvas id=\"scene\""},
		{"/brain", "text/html; charset=utf-8", "<canvas id=\"scene\""},
		{"/brain/brain.js", "text/javascript; charset=utf-8", "./vendor/three.module.min.js"},
		{"/brain/brain-core.js", "text/javascript; charset=utf-8", "export"},
		{"/brain/vendor/three.module.min.js", "text/javascript; charset=utf-8", "./three.core.min.js"},
		{"/brain/vendor/three.core.min.js", "text/javascript; charset=utf-8", "REVISION"},
		{"/brain/vendor/LICENSE.three", "text/plain; charset=utf-8", "MIT"},
	} {
		rec := do(t, s, http.MethodGet, tc.path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.ctype {
			t.Fatalf("%s: content-type %q want %q", tc.path, got, tc.ctype)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: body lacks %q", tc.path, tc.want)
		}
	}
	rec := do(t, s, http.MethodGet, "/brain/vendor/../brain.html", "")
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<canvas") {
		t.Fatal("path traversal must not serve page files through the vendor route")
	}
	if rec := do(t, s, http.MethodGet, "/brain/vendor/nope.js", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing vendor file: status %d", rec.Code)
	}
}
