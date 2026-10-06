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
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/google/go-containerregistry/pkg/authn"
)

type ACRKeychain struct {
	host          string
	credential    azcore.TokenCredential
	client        *azcontainerregistry.AuthenticationClient
	mutex         sync.Mutex
	authenticator authn.Authenticator
	expiresAt     time.Time
}

func NewACRKeychain(endpoint string, credential azcore.TokenCredential) (*ACRKeychain, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("ACR endpoint must be an HTTPS registry origin without credentials")
	}
	client, err := azcontainerregistry.NewAuthenticationClient(endpoint, nil)
	if err != nil {
		return nil, err
	}
	return &ACRKeychain{host: parsed.Host, credential: credential, client: client}, nil
}

func (keychain *ACRKeychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	candidates, err := keychain.Candidates(ctx, resource)
	if err != nil {
		return nil, err
	}
	return candidates[0], nil
}

func (keychain *ACRKeychain) Candidates(ctx context.Context, resource authn.Resource) ([]authn.Authenticator, error) {
	if resource.RegistryStr() != keychain.host {
		return []authn.Authenticator{authn.Anonymous}, nil
	}
	keychain.mutex.Lock()
	defer keychain.mutex.Unlock()
	if keychain.authenticator != nil && keychain.expiresAt.After(time.Now().Add(5*time.Minute)) {
		return []authn.Authenticator{keychain.authenticator}, nil
	}
	token, err := keychain.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://containerregistry.azure.net/.default"}})
	if err != nil {
		return nil, err
	}
	tenant := os.Getenv("AZURE_TENANT_ID")
	response, err := keychain.client.ExchangeAADAccessTokenForACRRefreshToken(ctx, azcontainerregistry.PostContentSchemaGrantTypeAccessToken, keychain.host,
		&azcontainerregistry.AuthenticationClientExchangeAADAccessTokenForACRRefreshTokenOptions{AccessToken: &token.Token, Tenant: &tenant})
	if err != nil {
		return nil, err
	}
	if response.RefreshToken == nil || *response.RefreshToken == "" {
		return nil, fmt.Errorf("ACR returned an empty refresh token")
	}
	keychain.authenticator = authn.FromConfig(authn.AuthConfig{Username: "00000000-0000-0000-0000-000000000000", Password: *response.RefreshToken})
	keychain.expiresAt = token.ExpiresOn
	return []authn.Authenticator{keychain.authenticator}, nil
}
