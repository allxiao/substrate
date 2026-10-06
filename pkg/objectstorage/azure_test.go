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

package objectstorage

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func TestAzureReadAndAbsence(t *testing.T) {
	for _, code := range []int{200, 404, 403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Length", "7")
				writer.WriteHeader(code)
				_, _ = io.WriteString(writer, "history")
			}))
			defer server.Close()
			client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{
				ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := NewAzureClient(client).GetObject(t.Context(), "actors", "home")
			if code == 200 {
				if err != nil {
					t.Fatal(err)
				}
				defer body.Close()
				data, err := io.ReadAll(body)
				if err != nil || string(data) != "history" {
					t.Fatalf("content = %q, error = %v", data, err)
				}
			} else if err == nil || errors.Is(err, ErrObjectNotFound) != (code == 404) {
				t.Fatalf("status %d: error = %v", code, err)
			}
		})
	}
}
