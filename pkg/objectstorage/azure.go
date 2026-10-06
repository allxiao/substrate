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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type azureClient struct {
	client *azblob.Client
}

func NewAzureClient(client *azblob.Client) ObjectStorage {
	return &azureClient{client: client}
}

func (a *azureClient) supportsStreamingPut() {}

func (a *azureClient) GetObject(ctx context.Context, container, object string) (io.ReadCloser, error) {
	response, err := a.client.DownloadStream(ctx, container, object, nil)
	if err != nil {
		if failure, ok := errors.AsType[*azcore.ResponseError](err); ok && failure.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: Azure container %q object %q", ErrObjectNotFound, container, object)
		}
		return nil, err
	}
	return response.Body, nil
}

func (a *azureClient) PutObject(ctx context.Context, container, object string, reader io.Reader) error {
	_, err := a.client.UploadStream(ctx, container, object, reader, &azblob.UploadStreamOptions{
		BlockSize: 4 << 20, Concurrency: 2,
	})
	return err
}
