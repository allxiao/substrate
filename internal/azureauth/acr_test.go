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

package azureauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/google/go-containerregistry/pkg/name"
)

type testCredential struct{}

func TestBlobEndpointRejectsCredentialsAndNonOrigin(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.com", "https://user:password@example.com", "https://example.com?sig=secret", "https://example.com#fragment", "https://example.com/extra"} {
		t.Setenv("ATE_AZURE_STORAGE_ENDPOINT", endpoint)
		if _, _, err := NewBlobClient(); err == nil || !strings.Contains(err.Error(), "ATE_AZURE_STORAGE_ENDPOINT") {
			t.Fatalf("invalid Blob endpoint accepted: %v", err)
		}
	}
}

func (testCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-aad", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestACRKeychainCachesExchangeAndScopesRegistry(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		if request.Form.Get("access_token") != "test-aad" || request.Form.Get("grant_type") != "access_token" {
			t.Error("invalid token exchange")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"refresh_token":"test-acr"}`))
	}))
	defer server.Close()
	keychain, err := NewACRKeychain(server.URL, testCredential{})
	if err != nil {
		t.Fatal(err)
	}
	keychain.client, err = azcontainerregistry.NewAuthenticationClient(server.URL, &azcontainerregistry.AuthenticationClientOptions{ClientOptions: azcore.ClientOptions{Transport: server.Client()}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := name.NewRegistry(keychain.host)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		auth, err := keychain.Resolve(registry)
		if err != nil {
			t.Fatal(err)
		}
		config, err := auth.Authorization()
		if err != nil || config.Password != "test-acr" {
			t.Fatalf("unexpected authorization: %v", err)
		}
	}
	other, _ := name.NewRegistry("other.example")
	if _, err := keychain.Resolve(other); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("token exchange called %d times", calls)
	}
}
