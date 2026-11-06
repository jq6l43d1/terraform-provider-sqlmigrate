// Package bundle uploads a source directory's files to GCS in the layout the
// runner expects.
package bundle

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/migrations"
)

// Store is the subset of Cloud Storage the upload needs.
type Store interface {
	Exists(ctx context.Context, bucket, object string) (bool, error)
	Put(ctx context.Context, bucket, object string, data []byte, contentType string) error
}

// Upload writes src's files and then its index. Bundles are content
// addressed, so an existing index means the bundle is already complete and
// nothing is written. It reports whether anything was uploaded.
func Upload(ctx context.Context, store Store, bucket, prefix string, src *migrations.Source, now time.Time) (bool, error) {
	indexObject := manifest.BundleIndexObject(prefix, src.BundleID)
	exists, err := store.Exists(ctx, bucket, indexObject)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	idx := manifest.BundleIndex{Schema: manifest.Schema, BundleID: src.BundleID, CreatedAt: now.UTC(), Files: []manifest.BundleFile{}}
	for _, f := range src.Files {
		if err := store.Put(ctx, bucket, manifest.BundleFileObject(prefix, src.BundleID, f.Path), f.Content, "application/sql"); err != nil {
			return false, err
		}
		idx.Files = append(idx.Files, manifest.BundleFile{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return false, err
	}
	if err := store.Put(ctx, bucket, indexObject, data, "application/json"); err != nil {
		return false, err
	}
	return true, nil
}
