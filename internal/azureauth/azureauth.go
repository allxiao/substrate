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
	"fmt"
	"net/url"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func NewCredential(clientID string) (azcore.TokenCredential, error) {
	return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{ClientID: clientID})
}

func NewBlobClient() (*azblob.Client, azcore.TokenCredential, error) {
	endpoint := os.Getenv("ATE_AZURE_STORAGE_ENDPOINT")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, nil, fmt.Errorf("ATE_AZURE_STORAGE_ENDPOINT must be an HTTPS account endpoint without credentials, query, or fragment")
	}
	credential, err := NewCredential(os.Getenv("ATE_AZURE_STORAGE_CLIENT_ID"))
	if err != nil {
		return nil, nil, err
	}
	client, err := azblob.NewClient(endpoint, credential, nil)
	return client, credential, err
}
