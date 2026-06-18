package images

import (
	"path"

	"github.com/acksell/clank/pkg/blobstore"
)

// KeyForImage builds the storage key for a user's uploaded image:
//
//	<userID>/images/<imageID>
//
// Images live in a dedicated bucket (separate from sync), so the prefix
// is tenant-scoping/defense-in-depth rather than collision avoidance.
// userID MUST come from authenticated token claims; imageID is a
// server-minted ULID. Both are validated for path safety.
func KeyForImage(userID, imageID string) (string, error) {
	if err := blobstore.ValidateComponent("userID", userID); err != nil {
		return "", err
	}
	if err := blobstore.ValidateComponent("imageID", imageID); err != nil {
		return "", err
	}
	return path.Join(userID, "images", imageID), nil
}
