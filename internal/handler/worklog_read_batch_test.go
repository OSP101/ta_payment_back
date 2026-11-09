package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

func TestWorklogReadBatchRejectsUnownedAssignment(t *testing.T) {
	pool:=testutil.NewPool(t)
	ctx:=context.Background()
	store,err:=storage.NewLocal(t.TempDir())
	if err!=nil { t.Fatal(err) }
	svc:=service.NewContainer(pool,store,mail.New(config.Config{}),audit.New(pool),config.Config{},nil,nil)
	ta, outsider, lecturer, term, course, section, request, assignment := uuid.New(),uuid.New(),uuid.New(),uuid.New(),uuid.New(),uuid.New(),uuid.New(),uuid.New()
	exec:=func(sql string,args ...any) { t.Helper(); if _,err:=pool.Exec(ctx,sql,args...); err!=nil { t.Fatal(err) } }
	for _,id:=range []uuid.UUID{ta,outsider,lecturer} { exec(`INSERT INTO users(id,email,first_name,last_name) VALUES($1,$2,'synthetic','batch')`,id,id.String()+"@example.test") }
	exec(`INSERT INTO academic_terms(id,academic_year,semester) VALUES($1,2569,1)`,term)
	exec(`INSERT INTO teaching_courses(id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs) VALUES($1,$2,'BATCHTEST','synthetic','undergrad',3,3,0,0)`,course,term)
	exec(`INSERT INTO sections(id,teaching_course_id,sec_no,track) VALUES($1,$2,'1','regular')`,section,course)
	exec(`INSERT INTO ta_requests(id,teaching_course_id,lecturer_id,reimburse_scope,status) VALUES($1,$2,$3,'both','approved')`,request,course,lecturer)
	exec(`INSERT INTO ta_request_assignments(id,request_id,section_id,ta_id,level) VALUES($1,$2,$3,$4,'undergrad')`,assignment,request,section,ta)
	actor:=outsider
	app:=fiber.New(fiber.Config{ErrorHandler:ErrorHandler})
	app.Use(func(c *fiber.Ctx) error { c.Locals(string(CtxUserID),actor); c.Locals(string(CtxRoles),[]string{"lecturer"}); return c.Next() })
	h:=WorkLogHandler{Svc:svc}
	app.Get("/api/v1/worklog/read-batch",ConcurrentEdits(pool,"synthetic-key"),h.ReadBatch)
	path:="/api/v1/worklog/read-batch?assignment_ids="+assignment.String()
	response,err:=app.Test(httptest.NewRequest("GET",path,nil),-1)
	if err!=nil { t.Fatal(err) };response.Body.Close()
	if response.StatusCode!=403 { t.Fatalf("unowned batch returned %d",response.StatusCode) }
	actor=ta // Service still enforces per-assignment ownership inside a batch.
	response,err=app.Test(httptest.NewRequest("GET",path,nil),-1)
	if err!=nil { t.Fatal(err) };response.Body.Close()
	if response.StatusCode!=200 || !strings.Contains(response.Header.Get("X-Edit-Views"),"course:*") { t.Fatal("authorized batch lost edit snapshot") }
	response,err=app.Test(httptest.NewRequest("GET","/api/v1/worklog/read-batch?assignment_ids="+strings.Repeat(assignment.String()+",",100),nil),-1)
	if err!=nil { t.Fatal(err) };response.Body.Close()
	if response.StatusCode!=400 { t.Fatal("oversized batch accepted") }
}
