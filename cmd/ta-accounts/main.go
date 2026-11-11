// ta-accounts imports ready TA identities and separately delivers credentials.
// Import never sends mail. Send defaults to preview; -send is a separate action.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/db"
	"ta-payment-back/internal/pii"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
)

const manifestAAD = "ta-payment/account-credentials/v1"

type manifest struct {
	Version   int                       `json:"version"`
	Batch     string                    `json:"batch"`
	Database  string                    `json:"database"`
	CreatedAt string                    `json:"created_at"`
	Records   []service.TAAccountRecord `json:"records"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ta-accounts:", err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "import", "import or send")
	input := flag.String("input", "", "ready account JSON file (import only)")
	path := flag.String("manifest", "data/account-imports/legacy-ta-20261111.enc", "encrypted credentials and delivery ledger")
	batch := flag.String("batch", "legacy-ta-20261111", "batch name")
	actorEmail := flag.String("actor-email", "", "active admin/staff email; optional when exactly one active admin exists")
	apply := flag.Bool("apply", false, "create accounts (does not send mail)")
	send := flag.Bool("send", false, "send pending credentials now (send mode only)")
	only := flag.String("only", "", "only this student ID in send mode")
	limit := flag.Int("limit", 0, "maximum messages to send; 0 means all pending")
	report := flag.String("report", "", "optional account status JSON report (never includes passwords)")
	flag.Parse()
	if (*mode != "import" && *mode != "send") || (*mode == "import" && *send) || (*mode == "send" && *apply) || *limit < 0 {
		return errors.New("invalid mode or flags")
	}
	cfg, err := config.Load()
	if err != nil {
		return errors.New("invalid application configuration")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot connect to configured database (credentials omitted)")
	}
	defer pool.Close()
	var database string
	if err = pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return err
	}
	endpoint, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid database endpoint")
	}
	target := endpoint.Host + "/" + database
	fmt.Printf("database=%s mode=%s\n", target, *mode)
	svc := service.NewTAAccountBatch(pool, cfg)
	actor, err := svc.ResolveActor(ctx, *actorEmail)
	if err != nil {
		return err
	}
	if *mode == "import" && !*apply {
		in, err := readInputs(*input)
		if err != nil {
			return err
		}
		records, err := svc.Import(ctx, actor, *batch, in, false, nil)
		if err != nil {
			return err
		}
		printCounts(records)
		if err = writeReport(*report, records); err != nil {
			return err
		}
		fmt.Println("PREVIEW: no accounts created; no email sent")
		return nil
	}
	key, err := storage.ParseKeyFromBase64(cfg.PIIEncKey)
	if err != nil {
		return errors.New("valid PII_ENC_KEY is required for encrypted credential storage")
	}
	cipher, err := pii.New(key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(*path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(*path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("batch is locked; check no other import/send is running before removing stale .lock")
	}
	lock.Close()
	defer os.Remove(*path + ".lock")
	if *mode == "import" {
		if _, err = os.Stat(*path); err == nil {
			return errors.New("manifest already exists; preserved existing credentials. Use import preview or send preview")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		in, err := readInputs(*input)
		if err != nil {
			return err
		}
		records, err := svc.Import(ctx, actor, *batch, in, true, func(records []service.TAAccountRecord) error {
			return saveManifest(*path, cipher, manifest{Version: 1, Batch: *batch, Database: target, CreatedAt: time.Now().UTC().Format(time.RFC3339), Records: records})
		})
		if err != nil {
			return err
		}
		printCounts(records)
		if err = writeReport(*report, records); err != nil {
			return err
		}
		fmt.Println("Accounts committed. Encrypted credentials saved. NO EMAIL SENT.")
		return nil
	}
	m, err := loadManifest(*path, cipher)
	if err != nil {
		return err
	}
	if m.Version != 1 || m.Database != target {
		return errors.New("manifest belongs to another database or version")
	}
	if *send {
		base, err := url.Parse(config.PrimaryBaseURL(cfg.AppBaseURL))
		if err != nil || base.Hostname() == "" || base.Scheme != "https" && base.Scheme != "http" || base.Hostname() == "localhost" || base.Hostname() == "127.0.0.1" || base.Hostname() == "::1" {
			return errors.New("set APP_BASE_URL to the actual recipient-accessible login URL before sending")
		}
		if strings.TrimSpace(cfg.SMTPHost) == "" {
			return errors.New("SMTP_HOST is not configured; no deliveries attempted")
		}
		fmt.Printf("login URL: %s/login\n", strings.TrimRight(base.String(), "/"))
	}
	sent, eligible, skipped := 0, 0, 0
	for i := range m.Records {
		r := &m.Records[i]
		if *only != "" && r.StudentID != *only {
			continue
		}
		if r.Status != "pending" {
			skipped++
			continue
		}
		if err = svc.ValidateCredential(ctx, *r); err != nil {
			skipped++
			fmt.Printf("%s: account changed/unavailable; skipped\n", r.StudentID)
			if *send {
				r.Status = "needs_review"
				r.Detail = "identity/password changed"
				r.Password = ""
				if err = saveManifest(*path, cipher, m); err != nil {
					return err
				}
			}
			continue
		}
		eligible++
		if !*send {
			continue
		}
		if *limit > 0 && sent >= *limit {
			break
		}
		// A crash or ambiguous SMTP result must not cause an automatic duplicate.
		r.Status = "sending"
		r.Detail = "delivery started " + time.Now().UTC().Format(time.RFC3339)
		if err = saveManifest(*path, cipher, m); err != nil {
			return err
		}
		if err = svc.Send(ctx, actor, *r); err != nil {
			r.Status = "needs_review"
			r.Detail = "delivery failed or outcome uncertain; inspect application audit/mail before retrying"
			if saveErr := saveManifest(*path, cipher, m); saveErr != nil {
				return saveErr
			}
			return fmt.Errorf("delivery stopped at %s; outcome recorded for review (details omitted)", r.StudentID)
		}
		r.Status = "sent"
		r.Detail = "sent " + time.Now().UTC().Format(time.RFC3339)
		r.Password = ""
		if err = saveManifest(*path, cipher, m); err != nil {
			return err
		}
		sent++
		fmt.Printf("%s: sent\n", r.StudentID)
	}
	fmt.Printf("eligible=%d sent=%d skipped=%d\n", eligible, sent, skipped)
	if !*send {
		fmt.Println("PREVIEW ONLY: no email sent. Run with -mode send -send on the meeting day.")
	}
	return nil
}
func readInputs(path string) ([]service.TAAccountInput, error) {
	if path == "" {
		return nil, errors.New("-input is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var in []service.TAAccountInput
	err = json.Unmarshal(data, &in)
	return in, err
}
func saveManifest(path string, c *pii.Cipher, m manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sealed, err := c.Seal([]byte(manifestAAD), raw)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".account-manifest-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(sealed); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func loadManifest(path string, c *pii.Cipher) (manifest, error) {
	var m manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	plain, err := c.Open([]byte(manifestAAD), raw)
	if err != nil {
		return m, errors.New("cannot decrypt manifest with configured PII_ENC_KEY")
	}
	err = json.Unmarshal(plain, &m)
	return m, err
}
func printCounts(records []service.TAAccountRecord) {
	counts := map[string]int{}
	for _, r := range records {
		counts[r.Status]++
	}
	fmt.Printf("total=%d ready=%d created_pending=%d existing=%d conflict=%d\n", len(records), counts["ready"], counts["pending"], counts["existing"], counts["conflict"])
}

func writeReport(path string, records []service.TAAccountRecord) error {
	if path == "" {
		return nil
	}
	safe := append([]service.TAAccountRecord(nil), records...)
	for i := range safe {
		safe[i].Password = ""
	}
	raw, err := json.MarshalIndent(safe, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
