package publish

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestClient returns a client pointed at a TLS test server, because Exists
// refuses a repository URL that is not absolute HTTPS.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := New()
	client.http.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	return client, server.URL
}

func TestExistsUsesV2PackageRouteWithBearerAuth(t *testing.T) {
	t.Parallel()
	var seen []string
	client, base := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" key="+r.Header.Get("X-NuGet-ApiKey"))
		if !strings.HasSuffix(r.URL.Path, "/9.9.9") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	present, err := client.Exists(t.Context(), base, "secret", "7zip", "24.09")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !present {
		t.Fatal("Exists() = false for a 200 response")
	}
	missing, err := client.Exists(t.Context(), base, "secret", "7zip", "9.9.9")
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if missing {
		t.Fatal("Exists() = true for a 404 response")
	}
	want := "HEAD /v2/package/7zip/24.09 auth=Bearer secret key=secret"
	if len(seen) == 0 || seen[0] != want {
		t.Fatalf("request = %q, want %q", seen, want)
	}
}

func TestPushUsesPutAndTreatsConflictAsSkipped(t *testing.T) {
	t.Parallel()
	var push string
	client, base := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		push = r.Method + " " + r.URL.Path + " content=" + r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusConflict)
	})
	pkgPath := filepath.Join(t.TempDir(), "7zip.24.09.nupkg")
	if err := os.WriteFile(pkgPath, []byte("package bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	skipped, err := client.Push(t.Context(), base, "secret", pkgPath, Package{ID: "7zip", Version: "24.09"})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if !skipped {
		t.Fatal("Push() = not skipped for a 409 response, want skipped")
	}
	if !strings.HasPrefix(push, "PUT /api/v2/package content=multipart/form-data") {
		t.Fatalf("push request = %q", push)
	}
}

func TestPushFailsLoudlyOnDeniedWrite(t *testing.T) {
	t.Parallel()
	client, base := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Error(w, "no", http.StatusForbidden)
	})
	pkgPath := filepath.Join(t.TempDir(), "7zip.24.09.nupkg")
	if err := os.WriteFile(pkgPath, []byte("package bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Push(t.Context(), base, "secret", pkgPath, Package{ID: "7zip", Version: "24.09"}); err == nil {
		t.Fatal("Push() error = nil for a 403 response")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("Push() error = %v, want the status code", err)
	}
}
