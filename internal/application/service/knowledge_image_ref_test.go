package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// A chunk image edit may only persist a reference that belongs to the chunk's
// own tenant. Otherwise the reference is later fed to knowledge delete, which
// removes the bytes from shared storage.
func TestValidateChunkImageURLRejectsCrossTenantLocalPath(t *testing.T) {
	err := validateChunkImageURL(
		context.Background(), "local://99/doc/image.png", 7, "doc", nil)
	require.Error(t, err, "a local:// path under another tenant must be rejected")
}

// The document segment must match the edited knowledge: another document's
// knowledge-owned object is not editable through this chunk.
func TestValidateChunkImageURLRejectsOtherDocumentsLocalPath(t *testing.T) {
	err := validateChunkImageURL(
		context.Background(), "local://7/other-doc/image.png", 7, "doc", nil)
	require.Error(t, err)
}

// storage://<backend>/local://... is the same physical layout and gets the
// same tenant/document checks.
func TestValidateChunkImageURLChecksStorageScopedLocalPath(t *testing.T) {
	require.Error(t, validateChunkImageURL(
		context.Background(),
		"storage://backend-a/local://99/doc/image.png", 7, "doc", nil))
	require.NoError(t, validateChunkImageURL(
		context.Background(),
		"storage://backend-a/local://7/doc/image.png", 7, "doc", nil))
}

// The same-tenant and knowledge-owned paths the edit form actually submits
// must keep working.
func TestValidateChunkImageURLAcceptsOwnTenantLocalPaths(t *testing.T) {
	for _, imageURL := range []string{
		"local://7/doc/image.png",
		"local://7/exports/extracted.png", // SaveBytes tenant scratch namespace
		"local://7/doc/sub/dir/image.png",
	} {
		require.NoError(t, validateChunkImageURL(context.Background(), imageURL, 7, "doc", nil), imageURL)
	}
}

// Remote references are stored verbatim and never turned into local bytes, so
// they stay allowed.
func TestValidateChunkImageURLAllowsRemoteURLs(t *testing.T) {
	for _, imageURL := range []string{
		"https://example.com/image.png",
		"http://example.com/image.png?x=1",
		"",
	} {
		require.NoError(t, validateChunkImageURL(context.Background(), imageURL, 7, "doc", nil), imageURL)
	}
}

// End-to-end wiring: the service rejects a forged cross-tenant reference
// before any chunk write happens.
func TestUpdateImageInfoRejectsCrossTenantImageReference(t *testing.T) {
	f := newDocumentWriteFixture(t)
	imageInfo, err := json.Marshal([]types.ImageInfo{{
		URL:         "local://99/victim/image.png",
		OriginalURL: "local://99/victim/image.png",
		Caption:     "rewritten",
	}})
	require.NoError(t, err)

	err = f.svc.UpdateImageInfo(f.ctx, "doc", "chunk", string(imageInfo))
	require.Error(t, err)
	f.requireNoWrites(t)
}
