//go:build integration

package invoices_test

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"trest.local/trest/clientgen"
)

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func command(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
}
func TestGeneratedGoClientAgainstWorkshop(t *testing.T) {
	app, _ := setup(t)
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	source, err := clientgen.Go(document, "invoiceclient")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", []byte("module workshop-client.test\n\ngo 1.23.0\n\nrequire trest.local/trest v0.0.0\nreplace trest.local/trest => "+strconv.Quote(root)+"\n"))
	writeFile(t, dir, "client.go", source)
	writeFile(t, dir, "client_test.go", []byte(workshopGoTest))
	command(t, dir, "go", "test", "-race", "./...")
}
func TestWorkshopTypeScriptClientMatchesAndExecutes(t *testing.T) {
	app, _ := setup(t)
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	generated, err := clientgen.TypeScript(document)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile("client/client.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, saved) {
		t.Fatal("generated client drift: run go run ./cmd/trest-demo -client-ts > examples/invoices/client/client.ts from project root")
	}
	tsc := os.Getenv("TREST_TSC")
	if tsc == "" {
		tsc, _ = exec.LookPath("tsc")
	}
	if tsc == "" {
		t.Skip("set TREST_TSC to verify the actual TypeScript app")
	}
	workshop, err := os.ReadFile("client/workshop.ts")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "client.ts", saved)
	writeFile(t, dir, "workshop.ts", workshop)
	writeFile(t, dir, "runtime.ts", []byte(workshopTypeScriptTest))
	command(t, dir, "node", tsc, "--strict", "--target", "ES2022", "--module", "commonjs", "--lib", "ES2022,DOM", "client.ts", "workshop.ts", "runtime.ts")
	server := httptest.NewServer(app)
	defer server.Close()
	t.Setenv("TREST_WORKSHOP_URL", server.URL)
	command(t, dir, "node", "runtime.js")
}

const workshopTypeScriptTest = `import {workshop} from './workshop';
declare const process:{env:Record<string,string|undefined>;exitCode:number};
workshop(process.env.TREST_WORKSHOP_URL!,'acme-test').then(result=>{
 if(result.attachment?.name!=='receipt.txt'||result.attachment.bytes!==22||result.count!==1||!result.snapshot.items?.some(invoice=>invoice.id===result.invoice.id))throw new Error('workshop typed upload or stream failed');
}).catch(error=>{console.error(error);process.exitCode=1;});
`

const workshopGoTest = `package invoiceclient
import("context";"errors";"net/http";"net/http/httptest";"strings";"testing";"trest.local/trest/examples/invoices")
type appTransport struct{handler http.Handler}
func(t appTransport)RoundTrip(r *http.Request)(*http.Response,error){recorder:=httptest.NewRecorder();t.handler.ServeHTTP(recorder,r);return recorder.Result(),nil}
func TestWorkshop(t *testing.T){
 app,err:=invoices.App(invoices.NewService(),map[string]string{"acme-test":"acme","globex-test":"globex"});if err!=nil{t.Fatal(err)}
 client,err:=NewClient("https://workshop.test",&http.Client{Transport:appTransport{handler:app}});if err!=nil{t.Fatal(err)};client.Headers.Set("Authorization","Bearer acme-test")
 var input CreateInvoiceInput;input.Body.CustomerId="customer-acme";input.Body.AmountMinor=100;input.Body.Currency="NGN"
 created,err:=client.CreateInvoice(context.Background(),input);if err!=nil{t.Fatal(err)};if created.Status!=201||created.Data.TenantId!="acme"{t.Fatalf("create: %#v",created)}
 id:=created.Data.Id
 got,err:=client.GetInvoice(context.Background(),GetInvoiceInput{Path:GetInvoicePath{Id:id}});if err!=nil||got.Data.Id!=id{t.Fatalf("get: %#v %v",got,err)}
 list,err:=client.ListInvoices(context.Background(),ListInvoicesInput{});if err!=nil||list.Data.Total!=1{t.Fatalf("list: %#v %v",list,err)}
 var update UpdateInvoiceInput;update.Path.Id=id;update.Body.AmountMinor=200;update.Body.Status="paid";updated,err:=client.UpdateInvoice(context.Background(),update);if err!=nil||updated.Data.Status!="paid"{t.Fatalf("update: %#v %v",updated,err)}
 attached,err:=client.UploadAttachment(context.Background(),UploadAttachmentInput{Path:UploadAttachmentPath{Id:id},Body:&UploadAttachmentBody{File:UploadFile{Filename:"proof.txt",ContentType:"text/plain",Reader:strings.NewReader("proof")}}});if err!=nil||attached.Data.Attachment==nil||attached.Data.Attachment.Bytes!=5{t.Fatalf("upload: %#v %v",attached,err)}
 stream,err:=client.InvoiceEvents(context.Background(),InvoiceEventsInput{});if err!=nil{t.Fatal(err)};if !stream.Next(){t.Fatalf("snapshot missing: %v",stream.Err())};snapshot:=stream.Event();if snapshot.Event!="snapshot"||snapshot.Data.Snapshot==nil||snapshot.Data.Snapshot.Items==nil||len(*snapshot.Data.Snapshot.Items)!=1||(*snapshot.Data.Snapshot.Items)[0].Id!=id{t.Fatalf("snapshot: %#v",snapshot)};if stream.Next()||stream.Err()!=nil{t.Fatalf("unexpected extra event: %v",stream.Err())};_=stream.Close()
 client.Headers.Set("Authorization","Bearer globex-test");_,err=client.GetInvoice(context.Background(),GetInvoiceInput{Path:GetInvoicePath{Id:id}});var denied *APIError;if !errors.As(err,&denied)||denied.Status!=403||denied.Code!="forbidden"{t.Fatalf("tenant: %#v %v",denied,err)}
 client.Headers.Set("Authorization","Bearer acme-test");deleted,err:=client.DeleteInvoice(context.Background(),DeleteInvoiceInput{Path:DeleteInvoicePath{Id:id}});if err!=nil||deleted.Status!=204{t.Fatalf("delete: %#v %v",deleted,err)}
}
`
