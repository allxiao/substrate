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

package main

import (
	"context"
	"time"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (service *AteomHerder) PreloadImage(ctx context.Context, request *ateletpb.PreloadImageRequest) (*ateletpb.PreloadImageResponse, error) {
	parsed, err := name.ParseReference(request.GetImage())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "image must be a valid digest-pinned OCI reference")
	}
	if _, ok := parsed.(name.Digest); !ok {
		return nil, status.Error(codes.InvalidArgument, "preload image must be digest pinned")
	}
	ttl := request.GetPinTtlSeconds()
	if request.GetPinOwner() == "" || ttl <= 0 || ttl > 86400 {
		return nil, status.Error(codes.InvalidArgument, "preload requires a pin owner and TTL from 1 to 86400 seconds")
	}
	if _, err := service.imageCache.EnsureImagePinned(ctx, request.Image, request.PinOwner, time.Duration(ttl)*time.Second); err != nil {
		return nil, err
	}
	return &ateletpb.PreloadImageResponse{}, nil
}
