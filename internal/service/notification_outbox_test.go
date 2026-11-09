package service

import (
	"bytes"
	"testing"
	"ta-payment-back/internal/pii"
)

func TestOutboxEncryptsPayloadAndFinalizesDelivery(t *testing.T) {
	s, user, ctx := notifyFixture(t)
	cipher,err := pii.New(bytes.Repeat([]byte{7},32))
	if err != nil { t.Fatal(err) }
	s.queueCipher = cipher
	if err := s.enqueue(ctx,user,"Synthetic notice","sensitive synthetic body","/ta",closingInform,MailLayout{}); err != nil { t.Fatal(err) }
	var raw []byte
	if err := s.pool.QueryRow(ctx,`SELECT payload_enc FROM notification_outbox WHERE user_id=$1`,user).Scan(&raw); err != nil { t.Fatal(err) }
	if bytes.Contains(raw,[]byte("sensitive synthetic body")) { t.Fatal("queue retained plaintext") }
	if s.countFor(t,ctx,user)!=1 { t.Fatal("enqueue did not atomically create bell notice") }
	if err := s.processNotice(ctx); err != nil { t.Fatal(err) }
	var state string; var cleared bool
	if err := s.pool.QueryRow(ctx,`SELECT state,payload_enc IS NULL FROM notification_outbox WHERE user_id=$1`,user).Scan(&state,&cleared); err != nil { t.Fatal(err) }
	if state!="sent" || !cleared { t.Fatal("completed job retained payload or wrong state") }
	if err := s.processNotice(ctx); err != nil { t.Fatal(err) }
	var delivered int
	if err := s.pool.QueryRow(ctx,`SELECT count(*) FROM notifications WHERE user_id=$1 AND channel='email'`,user).Scan(&delivered); err != nil { t.Fatal(err) }
	if delivered!=1 { t.Fatal("completed job was delivered twice") }
}

func TestOutboxFailureRetriesWithoutLosingBellNotice(t *testing.T) {
	s,user,ctx := notifyFixture(t)
	cipher,err := pii.New(bytes.Repeat([]byte{8},32))
	if err != nil { t.Fatal(err) }; s.queueCipher=cipher
	if err := s.enqueue(ctx,user,"Synthetic notice","body","/ta",closingInform,MailLayout{}); err != nil { t.Fatal(err) }
	// A bad ciphertext exercises failure without contacting any mail server.
	if _,err := s.pool.Exec(ctx,`UPDATE notification_outbox SET payload_enc=decode('00','hex'),attempts=4 WHERE user_id=$1`,user); err != nil { t.Fatal(err) }
	if err := s.processNotice(ctx); err != nil { t.Fatal(err) }
	var state string;var cleared bool
	if err := s.pool.QueryRow(ctx,`SELECT state,payload_enc IS NULL FROM notification_outbox WHERE user_id=$1`,user).Scan(&state,&cleared); err != nil { t.Fatal(err) }
	if state!="failed" || !cleared || s.countFor(t,ctx,user)!=1 { t.Fatal("failed job did not preserve in-app notice and clear encrypted payload") }
}
