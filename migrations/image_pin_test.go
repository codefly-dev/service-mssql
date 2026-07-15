package migrations

import "testing"

func TestAlembicImageIsImmutable(t *testing.T) {
	if alembicImage.Digest == "" {
		t.Fatalf("Alembic image is not pinned: %+v", alembicImage)
	}
}
