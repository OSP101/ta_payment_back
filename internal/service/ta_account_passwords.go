package service

import (
	"context"
	"github.com/google/uuid"
	"ta-payment-back/internal/auth"
)

type preparedAccountPassword struct{ password, hash string }

// Keep CPU-expensive hashing outside the account-creation transaction. The
// write phase repeats identity checks, so a concurrent account is never replaced.
func (s *TAAccountBatch) prepareAccountPasswords(ctx context.Context, actor uuid.UUID, batch string, in []TAAccountInput) (map[string]preparedAccountPassword, error) {
	preview, err := s.Import(ctx, actor, batch, in, false, nil)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, r := range preview {
		if r.Status == "ready" {
			total++
		}
	}
	out := make(map[string]preparedAccountPassword, total)
	if s.Progress != nil {
		s.Progress(0, total)
	}
	for _, r := range preview {
		if r.Status != "ready" {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		password := generateTempPassword(18)
		hash, err := auth.HashPassword(password)
		if err != nil {
			return nil, err
		}
		out[r.StudentID] = preparedAccountPassword{password: password, hash: hash}
		if s.Progress != nil {
			s.Progress(len(out), total)
		}
	}
	return out, nil
}
