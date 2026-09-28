package operator

import (
	"errors"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

func TestCatalogReadsAreBoundedFilteredSafeAndCursorStable(t *testing.T) {
	service, st, _, record := catalogManifestService(t)
	first := catalogManifestForArtifact(record)
	second := first
	second.Version = "2026.9.11"
	second.Artifact.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, manifest := range []appcatalog.Manifest{second, first} {
		if _, err := st.PublishCatalogManifest(manifest, "RAW_PUBLISHER_SENTINEL"); err != nil {
			t.Fatal(err)
		}
	}
	evaluatedAt := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	request := CatalogManifestListRequest{PackageID: "openclaw", Kind: appcatalog.KindApp, Limit: 1}
	page1, err := service.ListCatalogManifests(request, evaluatedAt)
	if err != nil || page1.Total != 2 || len(page1.Items) != 1 || page1.NextCursor == nil ||
		page1.Items[0].Manifest.Version != record.Version || page1.Items[0].PublishedBy != "" ||
		page1.SchemaVersion != CatalogReadSchemaVersion || page1.Consistency != CatalogReadConsistencyLive ||
		!page1.EvaluatedAt.Equal(evaluatedAt) {
		t.Fatalf("page1=%+v err=%v", page1, err)
	}
	request.Cursor = *page1.NextCursor
	page2, err := service.ListCatalogManifests(request, evaluatedAt)
	if err != nil || page2.Total != 2 || len(page2.Items) != 1 || page2.NextCursor != nil ||
		page2.Items[0].Manifest.Version != second.Version || page2.Items[0].PublishedBy != "" {
		t.Fatalf("page2=%+v err=%v", page2, err)
	}
	request.Kind = appcatalog.KindRuntime
	if _, err := service.ListCatalogManifests(request, evaluatedAt); !errors.Is(err, ErrInvalidCatalogRead) {
		t.Fatalf("cross-filter cursor err=%v", err)
	}
}

func TestMachineProfileReadsOrderNewestRevisionFirst(t *testing.T) {
	service, st, _, record := catalogManifestService(t)
	manifest := catalogManifestForArtifact(record)
	if _, err := st.PublishCatalogManifest(manifest, "RAW_PUBLISHER_SENTINEL"); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []int64{1, 3, 2} {
		profile := appcatalog.MachineProfile{
			SchemaVersion: appcatalog.SchemaVersion, ID: "standard", Revision: revision,
			Packages: []appcatalog.PackageRef{{PackageID: manifest.ID, Version: manifest.Version}},
		}
		if _, err := st.PublishMachineProfile(profile, "RAW_PUBLISHER_SENTINEL"); err != nil {
			t.Fatal(err)
		}
	}
	evaluatedAt := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	request := MachineProfileListRequest{ProfileID: "standard", Limit: 2}
	first, err := service.ListMachineProfiles(request, evaluatedAt)
	if err != nil || first.Total != 3 || len(first.Items) != 2 || first.NextCursor == nil ||
		first.Items[0].Profile.Revision != 3 || first.Items[1].Profile.Revision != 2 ||
		first.Items[0].PublishedBy != "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	request.Cursor = *first.NextCursor
	second, err := service.ListMachineProfiles(request, evaluatedAt)
	if err != nil || len(second.Items) != 1 || second.Items[0].Profile.Revision != 1 || second.NextCursor != nil {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestCatalogReadsRejectInvalidRequests(t *testing.T) {
	service, _, _, _ := catalogManifestService(t)
	for _, request := range []CatalogManifestListRequest{
		{PackageID: "bad/id"}, {Kind: "plugin"}, {Limit: MaxCatalogReadLimit + 1}, {Cursor: "bad"},
	} {
		if _, err := service.ListCatalogManifests(request, time.Now().UTC()); !errors.Is(err, ErrInvalidCatalogRead) {
			t.Fatalf("request=%+v err=%v", request, err)
		}
	}
	if _, err := service.ListMachineProfiles(MachineProfileListRequest{ProfileID: " bad"}, time.Now().UTC()); !errors.Is(err, ErrInvalidCatalogRead) {
		t.Fatalf("profile read err=%v", err)
	}
}
