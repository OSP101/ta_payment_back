package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestHeavyAdmissionRejectsBacklogAndReleasesCapacity(t *testing.T) {
	for i := 0; i < cap(heavyOperations); i++ { heavyOperations <- struct{}{} }
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	called := false
	app.Get("/export", heavyAdmission(func(c *fiber.Ctx) error { called = true; return c.SendStatus(204) }))
	response, err := app.Test(httptest.NewRequest("GET", "/export", nil))
	for i := 0; i < cap(heavyOperations); i++ { <-heavyOperations }
	if err != nil { t.Fatal(err) }
	response.Body.Close()
	if response.StatusCode != 503 || response.Header.Get("Retry-After") == "" || called { t.Fatal("busy export was admitted") }
	response, err = app.Test(httptest.NewRequest("GET", "/export", nil))
	if err != nil { t.Fatal(err) }; response.Body.Close()
	if response.StatusCode != 204 || !called || len(heavyOperations) != 0 { t.Fatal("export capacity was not released") }
}

func TestPayoutArchiveRejectsOverflowWithoutPartialWrite(t *testing.T) {
	var buffer payoutArchiveBuffer
	block := make([]byte, 1024*1024)
	for i:=0; i<64; i++ { if _,err:=buffer.Write(block); err!=nil { t.Fatal(err) } }
	if n,err:=buffer.Write([]byte{1}); err==nil || n!=0 || buffer.Len()!=64*1024*1024 { t.Fatal("archive limit produced a partial write") }
}
