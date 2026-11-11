package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/service"
)

func TestManifestEncryptsPasswordsAndKeepsDeliveryState(t *testing.T) {
	c, err := pii.New(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "batch.enc")
	m := manifest{Version: 1, Batch: "test", Database: "synthetic", Records: []service.TAAccountRecord{{Status: "pending", Password: "SyntheticSecret123"}}}
	if err = saveManifest(path, c, m); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SyntheticSecret123")) {
		t.Fatal("plaintext password on disk")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credentials must be owner-only")
	}
	loaded, err := loadManifest(path, c)
	if err != nil || loaded.Records[0].Password != m.Records[0].Password {
		t.Fatalf("load: %v", err)
	}
	other, _ := pii.New(bytes.Repeat([]byte{9}, 32))
	if _, err = loadManifest(path, other); err == nil {
		t.Fatal("accepted wrong key")
	}
	loaded.Records[0].Status = "sent"
	loaded.Records[0].Password = ""
	if err = saveManifest(path, c, loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadManifest(path, c)
	if err != nil || loaded.Records[0].Password != "" || loaded.Records[0].Status != "sent" {
		t.Fatal("delivery ledger did not survive save")
	}
}

func TestReportOmitsPasswordsWithoutClearingPendingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	records := []service.TAAccountRecord{{Status: "pending", Password: "SyntheticSecret123"}}
	if err := writeReport(path, records); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SyntheticSecret123")) || bytes.Contains(raw, []byte(`"password"`)) {
		t.Fatal("report leaked a password")
	}
	if records[0].Password != "SyntheticSecret123" {
		t.Fatal("report destroyed credential needed for later delivery")
	}
}
