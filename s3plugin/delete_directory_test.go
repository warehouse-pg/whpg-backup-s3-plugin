package s3plugin

import (
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urfave/cli"
	"github.com/warehouse-pg/common-go-libs/testhelper"
)

func TestResolveDeleteDirectoryPrefix(t *testing.T) {
	tests := []struct {
		name          string
		folder        string
		requestedPath string
		want          string
		wantError     bool
	}{
		{
			name:          "scopes a relative directory to the configured folder",
			folder:        "team/backups",
			requestedPath: "20260805/run-1",
			want:          "team/backups/20260805/run-1/",
		},
		{
			name:          "keeps an already scoped directory",
			folder:        "team/backups",
			requestedPath: "team/backups/20260805/run-1",
			want:          "team/backups/20260805/run-1/",
		},
		{
			name:          "keeps a similar top-level prefix inside the configured folder",
			folder:        "team/backups",
			requestedPath: "team/backups-old/run-1",
			want:          "team/backups/team/backups-old/run-1/",
		},
		{
			name:          "preserves a trailing slash without duplicating it",
			folder:        "team/backups",
			requestedPath: "20260805/run-1/",
			want:          "team/backups/20260805/run-1/",
		},
		{
			name:          "collapses redundant separators",
			folder:        "team/backups",
			requestedPath: "20260805//run-1///",
			want:          "team/backups/20260805/run-1/",
		},
		{
			name:          "rejects the bucket root",
			folder:        "team/backups",
			requestedPath: "/",
			wantError:     true,
		},
		{
			name:          "rejects an empty path",
			folder:        "team/backups",
			requestedPath: "",
			wantError:     true,
		},
		{
			name:          "rejects the current directory",
			folder:        "team/backups",
			requestedPath: ".",
			wantError:     true,
		},
		{
			name:          "rejects an absolute path",
			folder:        "team/backups",
			requestedPath: "/unrelated/run-1",
			wantError:     true,
		},
		{
			name:          "rejects the configured folder root",
			folder:        "team/backups",
			requestedPath: "team/backups",
			wantError:     true,
		},
		{
			name:          "rejects a relative traversal",
			folder:        "team/backups",
			requestedPath: "../unrelated/run-1",
			wantError:     true,
		},
		{
			name:          "rejects a traversal from an apparently scoped path",
			folder:        "team/backups",
			requestedPath: "team/backups/../../unrelated/run-1",
			wantError:     true,
		},
		{
			name:          "rejects a root configured folder",
			folder:        "/",
			requestedPath: "run-1",
			wantError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDeleteDirectoryPrefix(tt.folder, tt.requestedPath)
			if tt.wantError {
				if err == nil {
					t.Fatalf("resolveDeleteDirectoryPrefix(%q, %q) returned %q, expected an error", tt.folder, tt.requestedPath, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveDeleteDirectoryPrefix(%q, %q) returned an unexpected error: %v", tt.folder, tt.requestedPath, err)
			}
			if got != tt.want {
				t.Fatalf("resolveDeleteDirectoryPrefix(%q, %q) returned %q, expected %q", tt.folder, tt.requestedPath, got, tt.want)
			}
		})
	}
}

func TestDeleteDirectoryRejectsEmptyPathBeforeReadingConfig(t *testing.T) {
	flags := flag.NewFlagSet("testing flagset", flag.ContinueOnError)
	if err := flags.Parse([]string{"missing-config", ""}); err != nil {
		t.Fatalf("failed to parse test arguments: %v", err)
	}

	err := DeleteDirectory(cli.NewContext(nil, flags, nil))
	if err == nil || !strings.Contains(err.Error(), "non-empty directory path") {
		t.Fatalf("DeleteDirectory() returned %v, expected the empty path validation error", err)
	}
}

func TestDeleteDirectoryUsesScopedPrefix(t *testing.T) {
	var requestMu sync.Mutex
	var requestCount int
	var requestedPrefix string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		requestCount++
		requestedPrefix = r.URL.Query().Get("prefix")
		requestMu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()

	configPath := writeDeleteDirectoryConfig(t, server.URL)
	testStdout, _, _ := testhelper.SetupTestLogger()

	err := DeleteDirectory(newDeleteDirectoryContext(t, configPath, "20260805/run-1"))
	if err != nil {
		t.Fatalf("DeleteDirectory() returned an unexpected error: %v", err)
	}
	requestMu.Lock()
	gotRequestCount := requestCount
	gotRequestedPrefix := requestedPrefix
	requestMu.Unlock()
	if gotRequestCount != 1 {
		t.Fatalf("DeleteDirectory() sent %d S3 requests, expected 1", gotRequestCount)
	}
	if gotRequestedPrefix != "team/backups/20260805/run-1/" {
		t.Fatalf("DeleteDirectory() used S3 prefix %q, expected %q", gotRequestedPrefix, "team/backups/20260805/run-1/")
	}
	if logOutput := string(testStdout.Contents()); !strings.Contains(logOutput, "Deleting directory s3://test-bucket/team/backups/20260805/run-1/") {
		t.Fatalf("DeleteDirectory() did not log the fully resolved S3 prefix; log output: %q", logOutput)
	}
}

// TestDeleteDirectoryDoesNotMatchSiblingDirectories serves a bucket whose listing is filtered by
// the requested prefix, so deleting "run-1/" must leave the sibling directories "run-10/" and
// "run-1-old/" untouched.
func TestDeleteDirectoryDoesNotMatchSiblingDirectories(t *testing.T) {
	bucketKeys := []string{
		"team/backups/20260805/run-1/backup.gz",
		"team/backups/20260805/run-1/metadata.sql",
		"team/backups/20260805/run-10/backup.gz",
		"team/backups/20260805/run-1-old/backup.gz",
	}

	var mu sync.Mutex
	var deletedKeys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")

		if r.URL.Query().Has("delete") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("failed to read the delete request body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var request struct {
				Objects []struct {
					Key string `xml:"Key"`
				} `xml:"Object"`
			}
			if err := xml.Unmarshal(body, &request); err != nil {
				t.Errorf("failed to parse the delete request body %q: %v", body, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			var response strings.Builder
			response.WriteString(`<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
			mu.Lock()
			for _, object := range request.Objects {
				deletedKeys = append(deletedKeys, object.Key)
				fmt.Fprintf(&response, "<Deleted><Key>%s</Key></Deleted>", object.Key)
			}
			mu.Unlock()
			response.WriteString(`</DeleteResult>`)
			_, _ = fmt.Fprint(w, response.String())
			return
		}

		prefix := r.URL.Query().Get("prefix")
		var listing strings.Builder
		listing.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated>`)
		for _, key := range bucketKeys {
			if strings.HasPrefix(key, prefix) {
				fmt.Fprintf(&listing, "<Contents><Key>%s</Key><Size>1</Size></Contents>", key)
			}
		}
		listing.WriteString(`</ListBucketResult>`)
		_, _ = fmt.Fprint(w, listing.String())
	}))
	defer server.Close()

	configPath := writeDeleteDirectoryConfig(t, server.URL)
	_, _, _ = testhelper.SetupTestLogger()

	// Pass the trailing slash the way an operator would; it must not widen the deletion.
	err := DeleteDirectory(newDeleteDirectoryContext(t, configPath, "20260805/run-1/"))
	if err != nil {
		t.Fatalf("DeleteDirectory() returned an unexpected error: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), deletedKeys...)
	mu.Unlock()
	sort.Strings(got)
	want := []string{
		"team/backups/20260805/run-1/backup.gz",
		"team/backups/20260805/run-1/metadata.sql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeleteDirectory() deleted %q, expected only the objects under run-1/: %q", got, want)
	}
}

func TestDeleteDirectoryRejectsUnsafePathsWithoutCallingS3(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	configPath := writeDeleteDirectoryConfig(t, server.URL)
	unsafePaths := []string{
		"/",
		"/unrelated/run-1",
		"team/backups",
		"../unrelated/run-1",
		"team/backups/../../unrelated/run-1",
	}

	for _, unsafePath := range unsafePaths {
		t.Run(unsafePath, func(t *testing.T) {
			err := DeleteDirectory(newDeleteDirectoryContext(t, configPath, unsafePath))
			if err == nil {
				t.Fatalf("DeleteDirectory(%q) succeeded, expected an error", unsafePath)
			}
		})
	}

	if got := requestCount.Load(); got != 0 {
		t.Fatalf("DeleteDirectory() sent %d S3 requests for unsafe paths, expected 0", got)
	}
}

func newDeleteDirectoryContext(t *testing.T, configPath, requestedPath string) *cli.Context {
	t.Helper()

	flags := flag.NewFlagSet("testing flagset", flag.ContinueOnError)
	if err := flags.Parse([]string{configPath, requestedPath}); err != nil {
		t.Fatalf("failed to parse test arguments: %v", err)
	}
	return cli.NewContext(nil, flags, nil)
}

func writeDeleteDirectoryConfig(t *testing.T, endpoint string) string {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "plugin-config.yaml")
	config := fmt.Sprintf(`executablepath: /tmp/gpbackup_s3_plugin
options:
  aws_access_key_id: test-access-key
  aws_secret_access_key: test-secret-key
  bucket: test-bucket
  encryption: off
  endpoint: %s
  folder: team/backups
  region: test-region
`, endpoint)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("failed to write test plugin config: %v", err)
	}
	return configPath
}
