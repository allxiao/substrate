// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package objectstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type azureTestCredential struct{}

func (azureTestCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-source-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestAzureStoreCopyListAndDelete(t *testing.T) {
	staged, committed, pages := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("ETag", `"source-version"`)
		writer.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		switch {
		case request.Method == http.MethodHead:
			writer.Header().Set("Content-Length", "3")
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodDelete:
			writer.WriteHeader(http.StatusNotFound)
		case request.URL.Query().Get("comp") == "list":
			pages++
			marker := "next"
			if pages == 2 {
				marker = ""
			}
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(writer, `<EnumerationResults><Blobs><Blob><Name>home-%d</Name></Blob></Blobs><NextMarker>%s</NextMarker></EnumerationResults>`, pages, marker)
		case request.URL.Query().Get("comp") == "block":
			if request.Header.Get("x-ms-copy-source-authorization") != "Bearer test-source-token" || request.Header.Get("x-ms-source-if-match") != `"source-version"` {
				t.Error("copy did not authorize and pin source version")
			}
			staged++
			writer.WriteHeader(http.StatusCreated)
		case request.URL.Query().Get("comp") == "blocklist":
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), "Latest") {
				t.Error("empty committed copy")
			}
			committed++
			writer.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewAzure(client, azureTestCredential{})
	objects, err := store.List(t.Context(), "actors", "home")
	if err != nil || len(objects) != 2 {
		t.Fatalf("list = %v, %v", objects, err)
	}
	if err := store.Delete(t.Context(), "actors", "missing"); err != nil {
		t.Fatal(err)
	}
	if err := store.Copy(t.Context(), "actors", "source", "actors", "copy"); err != nil {
		t.Fatal(err)
	}
	if staged != 1 || committed != 1 {
		t.Fatalf("copy stages/commits = %d/%d", staged, committed)
	}
}
