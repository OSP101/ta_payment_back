package main

import (
	"bytes"
	"encoding/json"
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

func TestStudentInputRequiresProgrammeAndActiveState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "students.json")
	in := []service.TAAccountInput{{StudentID: "693380001-1", Email: "synthetic@example.invalid", FirstName: "Synthetic", LastName: "Student", StudyLevel: "undergrad"}}
	raw, _ := json.Marshal(in)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInputs(path, true); err == nil {
		t.Fatal("accepted incomplete student metadata")
	}
	in[0].Department = "Computer Science (SC-CS)"
	active := false
	in[0].IsActive = &active
	raw, _ = json.Marshal(in)
	os.WriteFile(path, raw, 0600)
	got, err := readInputs(path, true)
	if err != nil || got[0].IsActive == nil || *got[0].IsActive {
		t.Fatal("lost explicit inactive state")
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
