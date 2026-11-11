//go:build integration

package clientgen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mujhtech/trest/clientgen"
)

func put(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func run(t *testing.T, dir, command string, args ...string) {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s failed: %v\n%s", command, err, out)
	}
}

func exerciseGeneratedClients(t *testing.T, document []byte, goTest, tsTest string) int {
	t.Helper()
	clients := 0
	t.Run("Go", func(t *testing.T) {
		source, err := clientgen.Go(document, "generated")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		put(t, dir, "go.mod", []byte("module generated.test/runtime\n\ngo 1.23.0\n"))
		put(t, dir, "client.go", source)
		put(t, dir, "client_test.go", []byte(goTest))
		run(t, dir, "go", "test", "-race", "./...")
		clients++
	})
	t.Run("TypeScript", func(t *testing.T) {
		tsc := os.Getenv("TREST_TSC")
		if tsc == "" {
			tsc, _ = exec.LookPath("tsc")
		}
		if tsc == "" {
			t.Skip("set TREST_TSC to run TypeScript compiler verification")
		}
		source, err := clientgen.TypeScript(document)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		put(t, dir, "client.ts", source)
		put(t, dir, "runtime.ts", []byte(tsTest))
		run(t, dir, "node", tsc, "--strict", "--target", "ES2022", "--module", "commonjs", "--lib", "ES2022,DOM", "client.ts", "runtime.ts")
		run(t, dir, "node", "runtime.js")
		clients++
	})
	return clients
}
func TestGeneratedGoClientCompilesAndExecutes(t *testing.T) {
	source, err := clientgen.Go(contract(t), "generated")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	put(t, dir, "go.mod", []byte("module generated.test/client\n\ngo 1.23.0\n"))
	put(t, dir, "client.go", source)
	put(t, dir, "client_test.go", []byte(goClientTest))
	run(t, dir, "go", "test", "-race", "./...")
	put(t, dir, "invalid.go", []byte("package generated\nvar invalid=GetInvoiceInput{Path:GetInvoicePath{Id:123}}\n"))
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("compiler accepted wrong path type: %s", out)
	}
	put(t, dir, "invalid.go", []byte("package generated\nvar invalid=UploadBody{File:\"not a file\"}\n"))
	cmd = exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("compiler accepted wrong upload type: %s", out)
	}
	put(t, dir, "invalid.go", []byte("package generated\nvar invalid=VariantStatus202Output{Id:\"wrong status shape\"}\n"))
	cmd = exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("compiler accepted the wrong response variant: %s", out)
	}
}
func TestGeneratedTypeScriptCompilesAndExecutes(t *testing.T) {
	tsc := os.Getenv("TREST_TSC")
	if tsc == "" {
		tsc, _ = exec.LookPath("tsc")
	}
	if tsc == "" {
		t.Skip("set TREST_TSC to run TypeScript compiler verification")
	}
	source, err := clientgen.TypeScript(contract(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	put(t, dir, "client.ts", source)
	put(t, dir, "runtime.ts", []byte(tsClientTest))
	run(t, dir, "node", tsc, "--strict", "--target", "ES2022", "--module", "commonjs", "--lib", "ES2022,DOM", "client.ts", "runtime.ts")
	run(t, dir, "node", "runtime.js")
}

const goClientTest = `package generated
import("bytes";"context";"encoding/json";"errors";"io";"net/http";"net/url";"strings";"testing")
type transport func(*http.Request)(*http.Response,error)
func(f transport)RoundTrip(r *http.Request)(*http.Response,error){return f(r)}
func response(status int,body string)*http.Response{return &http.Response{StatusCode:status,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(body))}}
func TestRecursiveModel(t *testing.T){
 tree:=SchemaNode{Label:"root",Parent:&SchemaNode{Label:"parent"},Children:[]SchemaNode{{Label:"child"}},Links:map[string]SchemaNode{"leaf":{Label:"leaf"}}}
 data,err:=json.Marshal(tree);if err!=nil{t.Fatal(err)}
 var decoded SchemaNode;if err=json.Unmarshal(data,&decoded);err!=nil{t.Fatal(err)}
 if decoded.Parent.Label!="parent"||decoded.Children[0].Label!="child"||decoded.Links["leaf"].Label!="leaf"{t.Fatalf("recursive model lost values: %#v",decoded)}
}
func TestClientBehavior(t *testing.T){
 calls:=0
 c,err:=NewClient("https://example.test/api",&http.Client{Transport:transport(func(r *http.Request)(*http.Response,error){calls++;if r.Header.Get("Authorization")!="Bearer test"{t.Fatal("lost configured header")};switch r.URL.Path {
 case "/api/invoices/a/b?":if r.URL.EscapedPath()!="/api/invoices/a%2Fb%3F"{t.Fatalf("path not escaped: %s",r.URL.EscapedPath())};if r.URL.Query().Get("limit")!="2"||len(r.URL.Query()["tag"])!=2||r.Header.Get("X-Scope")!="acme"{t.Fatalf("binding lost: %s %v",r.URL,r.Header)};return response(200,"{\"id\":\"a/b?\",\"amount\":12,\"currency\":\"NGN\",\"note\":null}"),nil
 case "/api/invoices":body,_:=io.ReadAll(r.Body);if r.Header.Get("Content-Type")!="application/json"||!strings.Contains(string(body),"\"currency\":\"USD\""){t.Fatalf("bad body: %s",body)};return response(201,"{\"id\":\"new\",\"amount\":10,\"currency\":\"USD\"}"),nil
 case "/api/invoices/remove":if r.Method!="DELETE"{t.Fatal(r.Method)};return response(204,""),nil
 case "/api/invoices/head":if r.Method!="HEAD"{t.Fatal(r.Method)};result:=response(200,"");result.Header.Set("ETag","invoice-v1");result.Header.Set("Content-Length","120");return result,nil
 case "/api/invoices/denied":return response(403,"{\"code\":\"forbidden\",\"message\":\"denied\",\"request_id\":\"trace\",\"fields\":[{\"path\":\"/id\",\"code\":\"owner\",\"message\":\"no\"}]}"),nil
 case "/api/events":return response(200,"data: one\n\n"),nil
 case "/api/login":body,_:=io.ReadAll(r.Body);if string(body)!="name=Ada+Lovelace"{t.Fatal(string(body))};return response(200,"hello"),nil
 default:t.Fatal(r.URL);return nil,nil
 }})});if err!=nil{t.Fatal(err)};c.Headers.Set("Authorization","Bearer test")
 limit:=int64(2);tags:=[]string{"x & y","z"};scope:="acme"
 got,err:=c.GetInvoice(context.Background(),GetInvoiceInput{Path:GetInvoicePath{Id:"a/b?"},Query:GetInvoiceQuery{Limit:&limit,Tag:&tags},Headers:GetInvoiceHeaders{XScope:&scope}});if err!=nil||got.Status!=200||got.Data.Id!="a/b?"||got.Data.Note!=nil{t.Fatalf("get: %#v %v",got,err)}
 var create CreateInvoiceInput;create.Body.Amount=10;create.Body.Currency="USD";created,err:=c.CreateInvoice(context.Background(),create);if err!=nil||created.Status!=201||created.Data.Id!="new"{t.Fatalf("post: %#v %v",created,err)}
 removed,err:=c.DeleteInvoice(context.Background(),DeleteInvoiceInput{Path:DeleteInvoicePath{Id:"remove"}});if err!=nil||removed.Status!=204{t.Fatalf("delete: %#v %v",removed,err)}
 head,err:=c.HeadInvoice(context.Background(),HeadInvoiceInput{Path:HeadInvoicePath{Id:"head"}});if err!=nil||head.Status!=200||head.Header.Get("ETag")!="invoice-v1"||head.Header.Get("Content-Length")!="120"{t.Fatalf("head: %#v %v",head,err)}
 _,err=c.GetInvoice(context.Background(),GetInvoiceInput{Path:GetInvoicePath{Id:"denied"}});var failure *APIError;if !errors.As(err,&failure)||failure.Status!=403||failure.Code!="forbidden"||failure.RequestID!="trace"||len(failure.Fields)!=1{t.Fatalf("error: %#v %v",failure,err)}
 raw,err:=c.Events(context.Background(),EventsInput{});if err!=nil{t.Fatal(err)};data,_:=io.ReadAll(raw.Body);raw.Body.Close();if string(data)!="data: one\n\n"{t.Fatal(string(data))}
 login,err:=c.Login(context.Background(),LoginInput{Body:&LoginBody{Name:"Ada Lovelace"}});if err!=nil||login.Data!="hello"{t.Fatalf("form: %#v %v",login,err)}
 native,err:=c.Login(context.Background(),LoginInput{RawBody:url.Values{"name":{"Ada Lovelace"}}});if err!=nil||native.Data!="hello"{t.Fatalf("native form: %#v %v",native,err)}
 if calls!=8{t.Fatal(calls)}
}
var fileReadFailure=errors.New("file read failed")
type brokenReader struct{}
func(brokenReader)Read([]byte)(int,error){return 0,fileReadFailure}
type endlessReader struct{read int}
func(r *endlessReader)Read(p []byte)(int,error){for i:=range p{p[i]='x'};r.read+=len(p);return len(p),nil}
func TestTypedMultipart(t *testing.T){
 calls:=0;var captured []byte;var contentType string
 client,err:=NewClient("https://example.test/api",&http.Client{Transport:transport(func(r *http.Request)(*http.Response,error){
  calls++;captured,_=io.ReadAll(r.Body);contentType=r.Header.Get("Content-Type");r.Body=io.NopCloser(bytes.NewReader(captured))
  if err:=r.ParseMultipartForm(1<<20);err!=nil{t.Fatal(err)};defer r.MultipartForm.RemoveAll()
  if r.FormValue("title")!="two"||r.FormValue("count")!="2"||strings.Join(r.MultipartForm.Value["tag"],",")!="first,second"{t.Fatalf("lost typed fields: %v",r.MultipartForm.Value)}
  files:=r.MultipartForm.File;if len(files["file"])!=1||len(files["files"])!=2{t.Fatalf("lost files: %v",files)}
  if files["file"][0].Filename!="proof\"one.txt"||files["file"][0].Header.Get("Content-Type")!="text/plain"{t.Fatal(files["file"][0])}
  file,err:=files["file"][0].Open();if err!=nil{t.Fatal(err)};data,err:=io.ReadAll(file);file.Close();if err!=nil||string(data)!="proof"{t.Fatalf("file body: %q %v",data,err)}
  return response(201,"{\"id\":\"upload\"}"),nil
 })});if err!=nil{t.Fatal(err)}
 fresh:=func()*UploadBody{tags:=[]string{"first","second"};count:=int64(2);return &UploadBody{Title:"two",Tag:&tags,Count:&count,File:UploadFile{Filename:"proof\"one.txt",ContentType:"text/plain",Reader:strings.NewReader("proof")},Files:[]UploadFile{{Filename:"a.txt",Reader:strings.NewReader("a")},{Filename:"b.txt",Reader:strings.NewReader("b")}}}}
 result,err:=client.Upload(context.Background(),UploadInput{Body:fresh()});if err!=nil||result.Data.Id!="upload"{t.Fatalf("typed upload: %#v %v",result,err)}
 result,err=client.Upload(context.Background(),UploadInput{RawBody:&MultipartBody{Reader:bytes.NewReader(captured),ContentType:contentType}});if err!=nil||result.Data.Id!="upload"{t.Fatalf("native upload: %#v %v",result,err)}
 for _,test:=range []struct{name string;mutate func(*UploadBody)}{
  {"filename_header",func(body *UploadBody){body.File.Filename="proof\r\nX-Evil: yes"}},
  {"content_type_header",func(body *UploadBody){body.File.ContentType="text/plain\r\nX-Evil: yes"}},
  {"reader_failure",func(body *UploadBody){body.File.Reader=brokenReader{}}},
  {"missing_reader",func(body *UploadBody){body.File.Reader=nil}},
 }{t.Run(test.name,func(t *testing.T){body:=fresh();test.mutate(body);_,err:=client.Upload(context.Background(),UploadInput{Body:body});if err==nil{t.Fatal("invalid upload reached transport")};if test.name=="reader_failure"&&!errors.Is(err,fileReadFailure){t.Fatalf("reader error lost: %v",err)}})}
 if _,err=client.Upload(context.Background(),UploadInput{Body:fresh(),RawBody:&MultipartBody{Reader:strings.NewReader(""),ContentType:"multipart/form-data; boundary=x"}});err==nil{t.Fatal("ambiguous body accepted")}
 if _,err=client.Upload(context.Background(),UploadInput{RawBody:&MultipartBody{Reader:strings.NewReader(""),ContentType:"multipart/form-data; boundary=x\r\nX-Evil: yes"}});err==nil{t.Fatal("bad native header accepted")}
 reader:=&endlessReader{};oversize:=fresh();oversize.File.Reader=reader;client.MaxRequestBytes=1024
 if _,err=client.Upload(context.Background(),UploadInput{Body:oversize});err==nil{t.Fatal("oversize upload accepted")};if reader.read>32<<10{t.Fatalf("unbounded upload read: %d",reader.read)}
 client.MaxRequestBytes=1<<20;ctx,cancel:=context.WithCancel(context.Background());cancel();if _,err=client.Upload(ctx,UploadInput{Body:fresh()});!errors.Is(err,context.Canceled){t.Fatalf("cancellation: %v",err)}
 if calls!=2{t.Fatalf("invalid upload reached transport: %d calls",calls)}
}
func TestDeclaredRedirectsAndNotModified(t *testing.T) {
 client,err:=NewClient("https://example.test/api",nil)
 if err!=nil{t.Fatal(err)}
 calls:=0
 client.HTTP.Transport=transport(func(r *http.Request)(*http.Response,error){
  calls++
  switch r.URL.Path {
  case "/api/redirect":result:=response(302,"moved");result.Header.Set("Location","/destination");return result,nil
  case "/api/redirect-empty":result:=response(307,"");result.Header.Set("Location","/destination");return result,nil
  case "/api/not-modified":result:=response(304,"");result.Header.Set("ETag","invoice-v1");return result,nil
  default:t.Fatalf("unexpected request or followed redirect: %s",r.URL);return nil,nil
  }
 })
 moved,err:=client.Redirect(context.Background(),RedirectInput{})
 if err!=nil||moved.Status!=302||moved.Header.Get("Location")!="/destination"||moved.Data!="moved"{t.Fatalf("redirect: %#v %v",moved,err)}
 empty,err:=client.RedirectEmpty(context.Background(),RedirectEmptyInput{})
 if err!=nil||empty.Status!=307||empty.Header.Get("Location")!="/destination"{t.Fatalf("empty redirect: %#v %v",empty,err)}
 cached,err:=client.NotModified(context.Background(),NotModifiedInput{})
 if err!=nil||cached.Status!=304||cached.Header.Get("ETag")!="invoice-v1"{t.Fatalf("not modified: %#v %v",cached,err)}
 if calls!=3{t.Fatal(calls)}
}
type trackedBody struct{io.Reader;closed bool}
func(b *trackedBody)Close()error{b.closed=true;return nil}
func TestResponseVariants(t *testing.T){
 var last *trackedBody
 client,err:=NewClient("https://example.test",&http.Client{CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse},Transport:transport(func(r *http.Request)(*http.Response,error){
  status:=201;data:="{\"id\":\"created\"}";switch r.URL.Query().Get("kind"){case "queued":status=202;data="{\"ticket\":\"job-1\"}";case "empty":status=204;data="";case "binary":status=206;data="bytes";case "redirect":status=302;data="moved"}
  last=&trackedBody{Reader:strings.NewReader(data)};return &http.Response{StatusCode:status,Header:http.Header{"X-Variant":[]string{"selected"}},Body:last},nil
 })});if err!=nil{t.Fatal(err)}
 for _,kind:=range []string{"created","queued","empty","binary","redirect"}{t.Run(kind,func(t *testing.T){
  result,err:=client.Variant(context.Background(),VariantInput{Query:VariantQuery{Kind:kind}});if err!=nil{t.Fatal(err)};if result.Header.Get("X-Variant")!="selected"{t.Fatal("lost response headers")}
  switch kind{
  case "created":value,ok:=result.As201();if !ok||value.Id!="created"||result.Status202!=nil{t.Fatalf("created: %#v",result)}
  case "queued":value,ok:=result.As202();if !ok||value.Ticket!="job-1"||result.Status201!=nil{t.Fatalf("queued: %#v",result)}
  case "empty":if _,ok:=result.As204();!ok{t.Fatalf("empty: %#v",result)}
  case "binary":value,ok:=result.As206();if !ok||last.closed{t.Fatal("native body closed before caller")};data,err:=io.ReadAll(value.Body);value.Body.Close();if err!=nil||string(data)!="bytes"{t.Fatalf("binary: %q %v",data,err)}
  case "redirect":value,ok:=result.As302();if !ok||value!="moved"{t.Fatalf("redirect: %#v",result)}
  }
  if !last.closed{t.Fatal("response body was not released")}
 })}
}
`
const tsClientTest = `import { Client, APIError, SchemaNode } from './client';
declare const process: { exitCode: number };
declare const selectedFiles: FileList;
function assert(value: unknown, message: string): asserts value { if (!value) throw new Error(message); }
let calls = 0;
let capturedForm: FormData;
const fetcher: typeof fetch = async (input, init) => {
 calls++; const url = new URL(String(input)); const headers = new Headers(init?.headers);
 assert(headers.get('Authorization') === 'Bearer test','lost authorization');
 assert(init?.redirect === 'manual','redirect following must be disabled');
 switch (url.pathname) {
 case '/api/invoices/a%2Fb%3F': assert(url.searchParams.get('limit')==='2' && url.searchParams.getAll('tag').length===2,'query'); assert(headers.get('X-Scope')==='acme','header'); return new Response(JSON.stringify({id:'a/b?',amount:12,currency:'NGN',note:null}),{status:200});
 case '/api/invoices': assert(headers.get('Content-Type')==='application/json','content type'); assert(JSON.parse(String(init?.body)).currency==='USD','body'); return new Response(JSON.stringify({id:'new',amount:10,currency:'USD'}),{status:201});
 case '/api/invoices/remove': return new Response(null,{status:204});
 case '/api/invoices/head': assert(init?.method==='HEAD','head method'); return new Response(null,{status:200,headers:{ETag:'invoice-v1','Content-Length':'120'}});
 case '/api/invoices/denied': return new Response(JSON.stringify({code:'forbidden',message:'denied',request_id:'trace',fields:[{path:'/id',code:'owner',message:'no'}]}),{status:403});
 case '/api/events': return new Response('data: one\n\n');
 case '/api/login': assert(String(init?.body)==='name=Ada+Lovelace','form'); return new Response('hello');
 case '/api/upload': {
  assert(init?.body instanceof FormData,'typed multipart body');capturedForm=init.body;
  assert(!headers.has('Content-Type'),'multipart boundary belongs to fetch');
  assert(capturedForm.get('title')==='two'&&capturedForm.get('count')==='2'&&capturedForm.getAll('tag').join(',')==='first,second','typed form fields');
  const file=capturedForm.get('file');assert(file instanceof Blob&&await file.text()==='proof','typed file');assert(capturedForm.getAll('files').length===2,'typed file array');
  return new Response('{"id":"upload"}',{status:201});
 }
 case '/api/redirect': return new Response('moved',{status:302,headers:{Location:'/destination'}});
 case '/api/redirect-empty': return new Response(null,{status:307,headers:{Location:'/destination'}});
 case '/api/not-modified': return new Response(null,{status:304,headers:{ETag:'invoice-v1'}});
 default: throw new Error('unexpected URL '+url);
 }
};
async function testResponseVariants(){
 const client=new Client('https://example.test',{fetch:async(input)=>{
  const kind=new URL(String(input)).searchParams.get('kind');let status=201;let body:string|null='{"id":"created"}';
  switch(kind){case 'queued':status=202;body='{"ticket":"job-1"}';break;case 'empty':status=204;body=null;break;case 'binary':status=206;body='bytes';break;case 'redirect':status=302;body='moved';break;}
  return new Response(body,{status,headers:{'X-Variant':'selected'}});
 }});
 for(const kind of ['created','queued','empty','binary','redirect']){
  const result=await client.variant({query:{kind}});assert(result.headers.get('X-Variant')==='selected','variant header');
  switch(result.status){
   case 201:assert(result.data.id==='created','created shape');break;
   case 202:assert(result.data.ticket==='job-1','queued shape');break;
   case 204:assert(result.data===undefined,'empty shape');break;
   case 206:assert(await result.data.text()==='bytes','native shape');break;
   case 302:assert(result.data==='moved','text shape');break;
  }
  if(false&&result.status===202){
   // @ts-expect-error status 202 exposes the queued response, not the created body
   result.data.id;
  }
 }
}
async function main() {
 await testResponseVariants();
 const tree:SchemaNode={label:'root',parent:null,children:[{label:'child',parent:null,children:[],links:{}}],links:{}};
 assert(JSON.parse(JSON.stringify(tree)).children[0].label==='child','recursive model');
 const client = new Client('https://example.test/api',{fetch:fetcher,headers:{Authorization:'Bearer test'}});
 const result=await client.getInvoice({path:{id:'a/b?'},query:{tag:['x & y','z'],limit:2},headers:{'X-Scope':'acme'}});assert(result.data.id==='a/b?'&&result.status===200,'get');
 const created=await client.createInvoice({body:{amount:10,currency:'USD'}});assert(created.status===201&&created.data.id==='new','create');
 const deleted=await client.deleteInvoice({path:{id:'remove'}});assert(deleted.status===204&&deleted.data===undefined,'delete');
 const head=await client.headInvoice({path:{id:'head'}});assert(head.status===200&&head.data===undefined&&head.headers.get('ETag')==='invoice-v1'&&head.headers.get('Content-Length')==='120','head');
 try {await client.getInvoice({path:{id:'denied'}});throw new Error('missing failure');}catch(error){assert(error instanceof APIError&&error.code==='forbidden'&&error.status===403&&error.requestId==='trace'&&error.fields.length===1,'error');}
 const response=await client.events();assert(await response.text()==='data: one\n\n','stream');
 const login=await client.login({body:{name:'Ada Lovelace'}});assert(login.data==='hello','form');
 const nativeLogin=await client.login({body:new URLSearchParams({name:'Ada Lovelace'})});assert(nativeLogin.data==='hello','native form');
 const upload=await client.upload({body:{title:'two',count:2,tag:['first','second'],file:new Blob(['proof'],{type:'text/plain'}),files:[new Blob(['a']),new Blob(['b'])]}});assert(upload.data.id==='upload','typed upload');
 const nativeUpload=await client.upload({body:capturedForm!});assert(nativeUpload.data.id==='upload','native upload');
 const moved=await client.redirect();assert(moved.status===302&&moved.data==='moved'&&moved.headers.get('Location')==='/destination','redirect');
 const empty=await client.redirectEmpty();assert(empty.status===307&&empty.data===undefined&&empty.headers.get('Location')==='/destination','empty redirect');
 const cached=await client.notModified();assert(cached.status===304&&cached.data===undefined&&cached.headers.get('ETag')==='invoice-v1','not modified');
 assert(calls===13,'calls');
 const opaqueClient = new Client('https://example.test',{fetch:async()=>new Proxy(Response.error(),{get(target,key){if(key==='type')return 'opaqueredirect';return Reflect.get(target,key,target);}})});
 try {await opaqueClient.redirect();throw new Error('missing opaque redirect failure');}catch(error){assert(error instanceof Error&&error.message.includes('opaque redirect'),'opaque redirect error');}
 if(false) {
 client.upload({body:{title:'selected files',file:new Blob(),files:selectedFiles}});
 // @ts-expect-error required upload fields cannot be omitted
 client.upload({body:{title:'missing files'}});
 // @ts-expect-error file fields accept blobs, not strings
 client.upload({body:{title:'wrong',file:'not a file',files:[]}});
 // @ts-expect-error form field names and types come from the server
 client.login({body:{name:123}});
 // @ts-expect-error recursive children keep their declared field types
 tree.children.push({label:123,parent:null,children:[],links:{}});
 // @ts-expect-error WebSocket routes require a native WebSocket client
 client.socket();
 // @ts-expect-error path IDs are strings
 client.getInvoice({path:{id:123}});
 // @ts-expect-error request body is required
 client.createInvoice({});
 // @ts-expect-error currency follows the server enum
 client.createInvoice({body:{amount:10,currency:'EUR'}});
 }
}
main().catch(error=>{console.error(error);process.exitCode=1;});
`
