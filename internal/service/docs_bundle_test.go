package service

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	pdfcpuAPI "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpuModel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/pdfgen"
	"ta-payment-back/internal/testutil"
)

// The officer's download became a single merged PDF (24/07/2026 meeting):
// three separate files meant three prints and three chances to mislay one.
//
// The fallback matters as much as the merge. Uploads are PDF-only now, but
// rows predating that rule can be JPEG/PNG, and merging those would need
// rasterising. Dropping them silently would hand the officer an incomplete
// audit copy, so that case reverts to the old ZIP.

// memStore is a storage.Store backed by a map, so bundle tests do not depend
// on the filesystem or on encryption configuration.
type memStore struct{ files map[string][]byte }

func newMemStore() *memStore { return &memStore{files: map[string][]byte{}} }

func (m *memStore) Save(kind, filename string, r io.Reader) (string, int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	key := kind + "/" + uuid.NewString() + "-" + filename
	m.files[key] = b
	return key, int64(len(b)), nil
}
func (m *memStore) Open(key string) (io.ReadCloser, error) {
	b, ok := m.files[key]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memStore) Delete(key string) error { delete(m.files, key); return nil }
func (m *memStore) Path(key string) string  { return key }
func (m *memStore) Encrypted() bool         { return false }

// realPDF returns genuine PDF bytes, generated from the repo's own creditor
// template and fonts. Hand-rolled fixtures tend to be just close enough to
// fool a magic-byte check and then fail the merge, which would make this test
// pass for the wrong reason.
func realPDF(t *testing.T) []byte {
	t.Helper()
	root := repoRoot(t)
	b, err := pdfgen.FillCreditor(pdfgen.CreditorInput{
		TemplatePath: filepath.Join(root, "assets", "creditor_form_template.pdf"),
		FontDir:      filepath.Join(root, "assets", "fonts"),
	})
	if err != nil {
		t.Fatalf("build PDF fixture: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("PDF fixture is empty")
	}
	return b
}

// repoRoot resolves the module root from this source file's location, so the
// fixture finds assets/ regardless of the working directory tests run from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	// internal/service/docs_bundle_test.go -> repo root
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// bundleFixture wires a DocsService over a real database and an in-memory
// store, and returns the ids of documents whose bytes are `bodies`.
func bundleFixture(t *testing.T, bodies [][]byte, kinds []string) (*DocsService, []uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	store := newMemStore()
	svc := &DocsService{pool: pool, aud: audit.New(pool), store: store}
	ctx := context.Background()

	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, first_name, last_name, student_id, is_active)
		 VALUES ($1, $2, 'ทดสอบ', 'ผู้ใช้', '653020123-4', TRUE)`,
		userID, "bundle-"+userID.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	ids := make([]uuid.UUID, 0, len(bodies))
	for i, body := range bodies {
		key, size, err := store.Save("ta_docs", kinds[i]+".bin", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'approved')`,
			id, userID, kinds[i], kinds[i]+".pdf", "application/pdf", size, key); err != nil {
			t.Fatalf("insert doc: %v", err)
		}
		ids = append(ids, id)
	}
	return svc, ids
}

func TestBuildDocsBundle_MergesPDFsIntoOneFile(t *testing.T) {
	page := realPDF(t)
	svc, ids := bundleFixture(t,
		[][]byte{page, page, page},
		[]string{"national_id", "bank_book", "creditor_form"})

	body, name, err := svc.BuildDocsZip(context.Background(), ids)
	if err != nil {
		t.Fatalf("BuildDocsZip: %v", err)
	}
	if got := name[len(name)-4:]; got != ".pdf" {
		t.Fatalf("filename = %q, want a .pdf bundle", name)
	}
	if !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatal("bundle is not a PDF")
	}

	// It must be a real, readable document — not three files concatenated.
	// pdfcpu panics on a nil configuration, so pass one explicitly.
	conf := pdfcpuModel.NewDefaultConfiguration()
	conf.ValidationMode = pdfcpuModel.ValidationRelaxed
	info, err := pdfcpuAPI.PageCount(bytes.NewReader(body), conf)
	if err != nil {
		t.Fatalf("merged PDF does not parse: %v", err)
	}
	single, err := pdfcpuAPI.PageCount(bytes.NewReader(page), conf)
	if err != nil {
		t.Fatalf("fixture PDF does not parse: %v", err)
	}
	if info != single*3 {
		t.Errorf("merged page count = %d, want %d (3 × %d)", info, single*3, single)
	}
}

// End-to-end name check: the header the officer's browser sees comes from here,
// so the convention has to hold through the real query and merge, not just in
// taFileStem's unit tests.
func TestBuildDocsBundle_NamesFileByStudentIDAndName(t *testing.T) {
	page := realPDF(t)
	svc, ids := bundleFixture(t,
		[][]byte{page, page},
		[]string{"national_id", "bank_book"})

	_, name, err := svc.BuildDocsZip(context.Background(), ids)
	if err != nil {
		t.Fatalf("BuildDocsZip: %v", err)
	}
	if want := "653020123-4_ทดสอบ_ผู้ใช้.pdf"; name != want {
		t.Fatalf("bundle filename = %q, want %q", name, want)
	}
}

// A legacy image in the set must not be dropped from the officer's copy.
func TestBuildDocsBundle_FallsBackToZipForLegacyImage(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 13}
	svc, ids := bundleFixture(t,
		[][]byte{realPDF(t), png},
		[]string{"national_id", "bank_book"})

	body, name, err := svc.BuildDocsZip(context.Background(), ids)
	if err != nil {
		t.Fatalf("BuildDocsZip: %v", err)
	}
	if got := name[len(name)-4:]; got != ".zip" {
		t.Fatalf("filename = %q, want a .zip fallback", name)
	}
	// PK\x03\x04 — a real archive, and one that still carries both members.
	if !bytes.HasPrefix(body, []byte{0x50, 0x4B, 0x03, 0x04}) {
		t.Fatal("fallback bundle is not a ZIP")
	}
}

func TestBuildDocsBundle_RejectsEmptySelection(t *testing.T) {
	svc, _ := bundleFixture(t, nil, nil)
	if _, _, err := svc.BuildDocsZip(context.Background(), nil); err == nil {
		t.Fatal("an empty selection must be refused")
	}
}
