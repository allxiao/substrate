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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/azureauth"
	"github.com/agent-substrate/substrate/internal/imagecache"
	"github.com/agent-substrate/substrate/internal/objectstore"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/tarutil"
	"github.com/agent-substrate/substrate/pkg/objectstorage"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func main() {
	mode := flag.String("mode", "storage", "Validate storage, postgres, or image from an Azure Workload Identity Pod.")
	image := flag.String("image", "", "Digest-pinned Actor image for image-cache validation.")
	cacheRoot := flag.String("cache-root", "/var/lib/ate/image-cache", "Node-local OCI cache directory.")
	assetDirectory := flag.String("asset-dir", "/assets", "Pinned amd64 runtime asset directory for upload-assets.")
	snapshotURI := flag.String("snapshot-uri", "", "Immutable Actor snapshot URI for inspect-snapshot.")
	worker := flag.String("worker", "", "Worker name for the native drain-worker verification action.")
	kubeconfig := flag.String("kubeconfig", "/tmp/substrate-azure-data-kubeconfig", "Dedicated verification kubeconfig.")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var err error
	switch *mode {
	case "storage":
		err = validateStorage(ctx)
	case "postgres":
		err = validatePostgres(ctx)
	case "image":
		err = validateImage(ctx, *image, *cacheRoot)
	case "upload-assets":
		err = uploadRuntimeAssets(ctx, *assetDirectory)
	case "inspect-snapshot":
		err = inspectActorSnapshot(ctx, *snapshotURI)
	case "drain-worker":
		if *worker == "" {
			err = fmt.Errorf("--worker is required")
			break
		}
		var client *ateclient.Client
		client, err = ateclient.NewClient(ctx, *kubeconfig, "", "", "", false)
		if err == nil {
			defer client.Close()
			_, err = client.DrainWorker(ctx, &ateapipb.DrainWorkerRequest{Worker: &ateapipb.ObjectRef{Name: *worker}})
			if err == nil {
				fmt.Printf("NATIVE_WORKER_DRAIN_OK worker=%s\n", *worker)
			}
		}
	default:
		err = fmt.Errorf("unknown verification mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func inspectActorSnapshot(ctx context.Context, location string) error {
	uri, err := resources.ParseSnapshotURI(location)
	if err != nil {
		return err
	}
	client, credential, err := azureauth.NewBlobClient()
	if err != nil {
		return err
	}
	store := objectstore.NewAzure(client, credential)
	storage := objectstorage.NewAzureClient(client)
	container, prefix, err := objectstore.BucketPrefix(uri.Prefix())
	if err != nil {
		return err
	}
	objects, err := store.List(ctx, container, prefix)
	if err != nil {
		return err
	}
	if len(objects) != 2 {
		return fmt.Errorf("Data snapshot contains %d objects, want tar and manifest only", len(objects))
	}
	for _, object := range objects {
		if object != prefix+"manifest.json" && object != prefix+"durable-dir.tar.zstd" {
			return fmt.Errorf("unexpected Data snapshot object %q", object)
		}
	}
	manifestURI, err := uri.ObjectURI("manifest.json")
	if err != nil {
		return err
	}
	manifest, err := objectstorage.FetchFromGCS(ctx, storage, manifestURI)
	if err != nil {
		return err
	}
	var record struct {
		Scope         string   `json:"scope"`
		ActorUID      string   `json:"actorUid"`
		SnapshotFiles []string `json:"snapshotFiles"`
	}
	if err := json.Unmarshal(manifest, &record); err != nil {
		return err
	}
	if record.Scope != "data" || record.ActorUID == "" || len(record.SnapshotFiles) != 1 || record.SnapshotFiles[0] != "durable-dir.tar" {
		return fmt.Errorf("invalid HOME Data snapshot manifest")
	}
	directory, err := os.MkdirTemp("", "inspect-actor-data-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	archive, err := os.Create(filepath.Join(directory, "data.tar"))
	if err != nil {
		return err
	}
	archiveURI, err := uri.ObjectURI("durable-dir.tar.zstd")
	if err != nil {
		archive.Close()
		return err
	}
	fetchErr := objectstorage.FetchFileFromGCSWithZstd(ctx, storage, archiveURI, archive)
	if err := errors.Join(fetchErr, archive.Close()); err != nil {
		return err
	}
	root := filepath.Join(directory, "restored")
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	if err := tarutil.Extract(filepath.Join(directory, "data.tar"), root); err != nil {
		return err
	}
	volumes, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(volumes) != 1 || volumes[0].Name() != "home" {
		return fmt.Errorf("snapshot contains non-HOME volumes")
	}
	history, err := os.ReadFile(filepath.Join(root, "home", "history.txt"))
	if err != nil {
		return err
	}
	queries := strings.Split(strings.TrimSpace(string(history)), "\n")
	for _, query := range queries {
		var text string
		if err := json.Unmarshal([]byte(query), &text); err != nil {
			return err
		}
	}
	fmt.Printf("NATIVE_AZURE_DATA_SNAPSHOT_OK actor_uid=%s queries=%d files=durable-dir.tar.zstd,manifest.json volumes=home no-memory=true\n", record.ActorUID, len(queries))
	return nil
}

var amd64RuntimeAssets = map[string]string{
	"cloud-hypervisor": "448af3d4e59b22c2987f7df94c213ad40fb53a10d437e42b5ee6c4fce7c29ecc",
	"virtiofsd":        "15b2e72a78cc08a9bd8a6943e89fb69c88cb3cbeb63069efceade835342ac7d4",
	"vmlinux":          "8e9dbc3a6e4c26adb089d23d31201edebce905c7f3e31aa67b32bdd41df1b86f",
	"rootfs.img":       "96497f64da1de9c7473fef46c3d29ddd0805d334731cf9d903a21a5b2c33cefb",
}

func uploadRuntimeAssets(ctx context.Context, directory string) error {
	client, _, err := azureauth.NewBlobClient()
	if err != nil {
		return err
	}
	storage := objectstorage.NewAzureClient(client)
	container := os.Getenv("ATE_AZURE_STORAGE_CONTAINER")
	if container == "" {
		return fmt.Errorf("ATE_AZURE_STORAGE_CONTAINER is required")
	}
	for name, expected := range amd64RuntimeAssets {
		file, err := os.Open(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, hashErr := io.Copy(digest, file)
		if hashErr != nil {
			file.Close()
			return hashErr
		}
		if hex.EncodeToString(digest.Sum(nil)) != expected {
			file.Close()
			return fmt.Errorf("runtime asset %s SHA-256 mismatch", name)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return err
		}
		object := "runtime/amd64/" + expected + "/" + name
		putErr := storage.PutObject(ctx, container, object, file)
		if err := errors.Join(putErr, file.Close()); err != nil {
			return err
		}
		reader, err := storage.GetObject(ctx, container, object)
		if err != nil {
			return err
		}
		digest.Reset()
		_, readErr := io.Copy(digest, reader)
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return err
		}
		if hex.EncodeToString(digest.Sum(nil)) != expected {
			return fmt.Errorf("uploaded runtime asset %s SHA-256 mismatch", name)
		}
		fmt.Printf("AZURE_RUNTIME_ASSET_OK name=%s sha256=%s\n", name, expected)
	}
	return nil
}

func validateStorage(ctx context.Context) error {
	client, credential, err := azureauth.NewBlobClient()
	if err != nil {
		return err
	}
	content := objectstorage.NewAzureClient(client)
	store := objectstore.NewAzure(client, credential)
	container := os.Getenv("ATE_AZURE_STORAGE_CONTAINER")
	if container == "" {
		return fmt.Errorf("ATE_AZURE_STORAGE_CONTAINER is required")
	}
	prefix := "verification/" + uuid.NewString() + "/"
	for _, name := range []string{"original", "copied", "durable-dir.tar.zstd", "manifest.json"} {
		defer store.Delete(context.WithoutCancel(ctx), container, prefix+name)
	}
	payload := "Azure HOME Data snapshot verification"
	if err := content.PutObject(ctx, container, prefix+"original", strings.NewReader(payload)); err != nil {
		return err
	}
	if err := store.Copy(ctx, container, prefix+"original", container, prefix+"copied"); err != nil {
		return err
	}
	body, err := content.GetObject(ctx, container, prefix+"copied")
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		return err
	}
	if string(data) != payload {
		return fmt.Errorf("copied Blob content mismatch")
	}
	if err := validateHomeSnapshot(ctx, content, container, prefix); err != nil {
		return err
	}
	objects, err := store.List(ctx, container, prefix)
	if err != nil {
		return err
	}
	if len(objects) != 4 {
		return fmt.Errorf("Blob listing returned %d objects", len(objects))
	}
	for _, object := range objects {
		if err := store.Delete(ctx, container, object); err != nil {
			return err
		}
	}
	if err := store.Delete(ctx, container, prefix+"original"); err != nil {
		return err
	}
	fmt.Println("AZURE_BLOB_OK streaming-put get paginated-list OAuth-server-side-copy idempotent-delete")
	return nil
}

func validateHomeSnapshot(ctx context.Context, storage objectstorage.ObjectStorage, container, prefix string) error {
	directory, err := os.MkdirTemp("", "azure-home-data-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	home := filepath.Join(directory, "durable", "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	history := "\"query-1\"\n\"query-2\"\n"
	if err := os.WriteFile(filepath.Join(home, "history.txt"), []byte(history), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "rootfs-only"), []byte("not durable"), 0o600); err != nil {
		return err
	}
	archive := filepath.Join(directory, "durable-dir.tar")
	if err := tarutil.Create(ctx, archive, filepath.Join(directory, "durable")); err != nil {
		return err
	}
	uri := "azblob://" + container + "/" + prefix
	if err := objectstorage.SendLocalFileToGCSWithZstd(ctx, storage, uri+"durable-dir.tar.zstd", archive); err != nil {
		return err
	}
	if err := objectstorage.SendBytesToGCS(ctx, storage, uri+"manifest.json", []byte(`{"scope":"data","snapshotFiles":["durable-dir.tar"]}`)); err != nil {
		return err
	}
	_, err = objectstorage.FetchFromGCS(ctx, storage, uri+"manifest.json")
	if err != nil {
		return err
	}
	download, err := os.Create(filepath.Join(directory, "restored.tar"))
	if err != nil {
		return err
	}
	fetchErr := objectstorage.FetchFileFromGCSWithZstd(ctx, storage, uri+"durable-dir.tar.zstd", download)
	if err := errors.Join(fetchErr, download.Close()); err != nil {
		return err
	}
	restored := filepath.Join(directory, "restored")
	if err := os.Mkdir(restored, 0o700); err != nil {
		return err
	}
	if err := tarutil.Extract(filepath.Join(directory, "restored.tar"), restored); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(restored, "home", "history.txt"))
	if err != nil || string(data) != history {
		return fmt.Errorf("HOME snapshot restoration failed")
	}
	if _, err := os.Stat(filepath.Join(restored, "rootfs-only")); !os.IsNotExist(err) {
		return fmt.Errorf("non-HOME sentinel entered the Data snapshot")
	}
	fmt.Println("AZURE_HOME_DATA_OK HOME-only-tar zstd-upload manifest-last download restore no-rootfs-state")
	return nil
}

func validatePostgres(ctx context.Context) error {
	credential, err := azureauth.NewCredential(os.Getenv("ATE_AZURE_POSTGRES_CLIENT_ID"))
	if err != nil {
		return err
	}
	for _, role := range []string{"substrate_readwrite", "substrate_owner"} {
		config, err := pgx.ParseConfig(os.Getenv("ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING"))
		if err != nil {
			return fmt.Errorf("invalid PostgreSQL DSN")
		}
		if config.TLSConfig == nil || config.TLSConfig.InsecureSkipVerify {
			return fmt.Errorf("PostgreSQL verification requires verified TLS")
		}
		token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://ossrdbms-aad.database.windows.net/.default"}})
		if err != nil {
			return err
		}
		config.Password = token.Token
		connection, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			return err
		}
		if _, err := connection.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			connection.Close(ctx)
			return err
		}
		var current string
		err = connection.QueryRow(ctx, "SELECT current_user").Scan(&current)
		closeErr := connection.Close(ctx)
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if current != role {
			return fmt.Errorf("unexpected PostgreSQL role")
		}
	}
	fmt.Println("AZURE_POSTGRES_OK Workload-Identity verified-TLS fresh-connections runtime-owner-roles")
	return nil
}

func validateImage(ctx context.Context, reference, root string) error {
	if reference == "" {
		return fmt.Errorf("--image is required")
	}
	credential, err := azureauth.NewCredential(os.Getenv("ATE_AZURE_ACR_CLIENT_ID"))
	if err != nil {
		return err
	}
	keychain, err := azureauth.NewACRKeychain(os.Getenv("ATE_AZURE_ACR_ENDPOINT"), credential)
	if err != nil {
		return err
	}
	cache, err := imagecache.New(root, imagecache.WithKeychain(keychain))
	if err != nil {
		return err
	}
	start := time.Now()
	image, err := cache.EnsureImagePinned(ctx, reference, "azure-validation", time.Hour)
	if err != nil {
		return err
	}
	first := time.Since(start)
	start = time.Now()
	if _, err := cache.EnsureImage(ctx, reference); err != nil {
		return err
	}
	fmt.Printf("AZURE_IMAGE_CACHE_OK node=%s digest=%s layers=%d first=%s warm=%s\n", os.Getenv("NODE_NAME"), image.Digest.String(), len(image.LayerDirs), first, time.Since(start))
	return nil
}
