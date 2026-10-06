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
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/google/uuid"
)

type azureStore struct {
	client     *azblob.Client
	credential azcore.TokenCredential
}

func NewAzure(client *azblob.Client, credential azcore.TokenCredential) Store {
	return &azureStore{client: client, credential: credential}
}

func (a *azureStore) List(ctx context.Context, container, prefix string) ([]string, error) {
	pager := a.client.NewListBlobsFlatPager(container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	var objects []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Segment.BlobItems {
			objects = append(objects, *item.Name)
		}
	}
	return objects, nil
}

func (a *azureStore) Delete(ctx context.Context, container, object string) error {
	_, err := a.client.DeleteBlob(ctx, container, object, nil)
	if failure, ok := errors.AsType[*azcore.ResponseError](err); ok && failure.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (a *azureStore) Copy(ctx context.Context, sourceContainer, sourceObject, destinationContainer, destinationObject string) error {
	source := a.client.ServiceClient().NewContainerClient(sourceContainer).NewBlobClient(sourceObject)
	destination := a.client.ServiceClient().NewContainerClient(destinationContainer).NewBlockBlobClient(destinationObject)
	properties, err := source.GetProperties(ctx, nil)
	if err != nil {
		return err
	}
	if properties.ContentLength == nil || *properties.ContentLength < 0 || a.credential == nil {
		return fmt.Errorf("azure copy requires source size and OAuth credentials")
	}
	token, err := a.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://storage.azure.com/.default"}})
	if err != nil {
		return err
	}
	authorization := "Bearer " + token.Token
	runID := uuid.NewString()
	var blocks []string
	const blockSize int64 = 16 << 20
	for offset := int64(0); offset < *properties.ContentLength; offset += blockSize {
		blockID := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s-%012d", runID, offset)))
		_, err := destination.StageBlockFromURL(ctx, blockID, source.URL(), &blockblob.StageBlockFromURLOptions{
			CopySourceAuthorization:        &authorization,
			Range:                          blob.HTTPRange{Offset: offset, Count: min(blockSize, *properties.ContentLength-offset)},
			SourceModifiedAccessConditions: &blob.SourceModifiedAccessConditions{SourceIfMatch: properties.ETag},
		})
		if err != nil {
			return err
		}
		blocks = append(blocks, blockID)
	}
	_, err = destination.CommitBlockList(ctx, blocks, nil)
	return err
}
