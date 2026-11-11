//go:build integration

package clientgen_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/clientgen"
)

type socketMessage struct {
	Text string `json:"text"`
}
type socketReply struct {
	Echo string `json:"echo"`
}

func TestGeneratedSocketClientsRoundTrip(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	app, err := trest.New(
		trest.WebSocketMessages[socketMessage, socketReply](trest.Raw("GET", "/chat", handler).Name("chat")),
		trest.WebSocketMessages[socketMessage, socketReply](trest.Raw("GET", "/private-chat/{room}", handler).Name("privateChat")),
	)
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	source, err := clientgen.Go(document, "generated")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	put(t, dir, "go.mod", []byte("module generated.test/socket\n\ngo 1.23.0\n\nrequire (\n github.com/mujhtech/trest v0.0.0\n github.com/mujhtech/trest/adapters/websocket v0.0.0\n)\nreplace github.com/mujhtech/trest => "+strconv.Quote(root)+"\nreplace github.com/mujhtech/trest/adapters/websocket => "+strconv.Quote(filepath.Join(root, "adapters/websocket"))+"\n"))
	sum, err := os.ReadFile(filepath.Join(root, "adapters/websocket/go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, dir, "go.sum", sum)
	put(t, dir, "client.go", source)
	put(t, dir, "client_test.go", []byte(socketGoTest))
	tsc := os.Getenv("TREST_TSC")
	if tsc == "" {
		tsc, _ = exec.LookPath("tsc")
	}
	if tsc != "" {
		source, err = clientgen.TypeScript(document)
		if err != nil {
			t.Fatal(err)
		}
		put(t, dir, "client.ts", source)
		put(t, dir, "runtime.ts", []byte(socketTSTest))
		run(t, dir, "node", tsc, "--strict", "--target", "ES2022", "--module", "commonjs", "--lib", "ES2022,DOM", "client.ts", "runtime.ts")
	} else {
		t.Log("TypeScript socket fixture skipped: set TREST_TSC")
	}
	run(t, dir, "go", "test", "-mod=mod", "-race", "./...")
	put(t, dir, "invalid.go", []byte("package generated\nvar invalid=ChatMessage{Text:123}\n"))
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("Go accepted an invalid socket message: %s", output)
	}
}

const socketGoTest = `package generated
import("context";"errors";"net/http";"net/http/httptest";"os";"os/exec";"strings";"testing";"time";"github.com/mujhtech/trest";ws "github.com/mujhtech/trest/adapters/websocket")
type incoming struct{Text string ` + "`json:\"text\"`" + `}
type outgoing struct{Echo string ` + "`json:\"echo\"`" + `}
func socketServer(t *testing.T)*httptest.Server{
 t.Helper();handler:=func(ctx context.Context,c *ws.JSONConn[incoming,outgoing])error{for{message,err:=c.Read(ctx);if err!=nil{return err};if err=c.Write(ctx,outgoing{Echo:message.Text});err!=nil{return err}}}
 public,err:=ws.NewJSON("/chat",ws.Options{Public:true,AllowMissingOrigin:true},handler);if err!=nil{t.Fatal(err)}
 private,err:=ws.NewJSON("/private-chat/{room}",ws.Options{Permission:"chat",AllowMissingOrigin:true},handler);if err!=nil{t.Fatal(err)}
 app,err:=trest.New(public,private,
  trest.Authenticate(func(r *http.Request)(trest.Principal,error){if r.Header.Get("Authorization")=="Bearer fixture"{return trest.Principal{ID:"alice"},nil};return trest.Principal{},nil}),
  trest.Authorization(trest.AuthorizerFunc(func(_ context.Context,p trest.Principal,permission string,_ any)error{if p.ID=="alice"&&permission=="chat"{return nil};return trest.NewError(trest.Forbidden,"denied",nil)})),
 );if err!=nil{t.Fatal(err)}
 server:=httptest.NewServer(app);t.Cleanup(server.Close);return server
}
func dialSocket(ctx context.Context,target string,headers http.Header)(SocketTransport,error){transport,_,err:=ws.DialJSONTransport(ctx,target,ws.ClientOptions{HTTPHeader:headers});return transport,err}
func TestSocket(t *testing.T){
 server:=socketServer(t);client,err:=NewClient(server.URL,nil);if err!=nil{t.Fatal(err)}
 if _,err=client.Chat(context.Background(),ChatInput{});err==nil{t.Fatal("missing dialer accepted")};client.SocketDialer=dialSocket
 socket,err:=client.Chat(context.Background(),ChatInput{});if err!=nil{t.Fatal(err)};defer socket.Close()
 if err=socket.Send(context.Background(),ChatMessage{Text:"hello café"});err!=nil{t.Fatal(err)}
 reply,err:=socket.Receive(context.Background());if err!=nil||reply.Echo!="hello café"{t.Fatalf("echo: %#v %v",reply,err)}
 if err=socket.Send(context.Background(),ChatMessage{Text:strings.Repeat("x",65<<10)});err==nil{t.Fatal("outbound size limit ignored")}
 if err=socket.Send(context.Background(),ChatMessage{Text:"after limit"});err!=nil{t.Fatal(err)};if reply,err=socket.Receive(context.Background());err!=nil||reply.Echo!="after limit"{t.Fatalf("limit damaged connection: %#v %v",reply,err)}
 ctx,cancel:=context.WithTimeout(context.Background(),20*time.Millisecond);defer cancel();if _,err=socket.Receive(ctx);!errors.Is(err,context.DeadlineExceeded){t.Fatalf("receive cancellation lost: %v",err)}
 if _,err=client.PrivateChat(context.Background(),PrivateChatInput{Path:PrivateChatPath{Room:"a/b"}});err==nil{t.Fatal("unauthenticated upgrade accepted")}
 client.Headers.Set("Authorization","Bearer fixture");private,err:=client.PrivateChat(context.Background(),PrivateChatInput{Path:PrivateChatPath{Room:"a/b"}});if err!=nil{t.Fatal(err)};defer private.Close()
 if err=private.Send(context.Background(),PrivateChatMessage{Text:"authenticated"});err!=nil{t.Fatal(err)};got,err:=private.Receive(context.Background());if err!=nil||got.Echo!="authenticated"{t.Fatalf("private echo: %#v %v",got,err)}
}
type badSocket struct{data []byte;closed bool}
func(s *badSocket)Read(context.Context)([]byte,error){return s.data,nil}
func(s *badSocket)Write(context.Context,[]byte)error{return nil}
func(s *badSocket)Close()error{s.closed=true;return nil}
func TestSocketRejectsMalformedMessages(t *testing.T){
 for _,tc:=range []struct{name string;data []byte}{{"JSON",[]byte("{broken}")},{"UTF8",[]byte{255}},{"size",[]byte(strings.Repeat("x",65<<10))}}{t.Run(tc.name,func(t *testing.T){
  transport:=&badSocket{data:tc.data};client,_:=NewClient("http://example.test",nil);client.SocketDialer=func(context.Context,string,http.Header)(SocketTransport,error){return transport,nil}
  socket,err:=client.Chat(context.Background(),ChatInput{});if err!=nil{t.Fatal(err)};if _,err=socket.Receive(context.Background());err==nil||!transport.closed{t.Fatalf("malformed message accepted or connection retained: %v",err)}
 })}
}
func TestSocketTypeScript(t *testing.T){
 if _,err:=os.Stat("runtime.js");errors.Is(err,os.ErrNotExist){t.Skip("TypeScript compiler unavailable")}
 server:=socketServer(t);cmd:=exec.Command("node","runtime.js");cmd.Env=append(os.Environ(),"TREST_SOCKET_URL="+server.URL)
 if output,err:=cmd.CombinedOutput();err!=nil{t.Fatalf("TypeScript socket client: %v\n%s",err,output)}
}
`

const socketTSTest = `import {Client,SocketConnection} from './client';
declare const process:{env:Record<string,string|undefined>;exitCode:number};
class FakeSocket extends EventTarget implements SocketConnection {
 readyState=0;bufferedAmount=0;closed=false;sent:string[]=[];
 open(){this.readyState=1;this.dispatchEvent(new Event('open'));}
 message(data:unknown){this.dispatchEvent(new MessageEvent('message',{data}));}
 send(data:string){this.sent.push(data);}
 close(){this.readyState=3;this.closed=true;}
}
async function mustReject(action:()=>Promise<unknown>,label:string){let failed=false;try{await action();}catch{failed=true;}if(!failed)throw new Error(label);}
async function main(){
 const client=new Client(process.env.TREST_SOCKET_URL!);const socket=await client.chat();const iterator=socket.messages();
 if(false){
  // @ts-expect-error typed outgoing messages reject numeric text
  socket.send({text:123});
 }
 socket.send({text:'hello café'});const reply=await iterator.next();if(reply.done||reply.value.echo!=='hello café')throw new Error('native WebSocket echo changed');
 let limited=false;try{socket.send({text:'x'.repeat(65*1024)});}catch{limited=true;}if(!limited)throw new Error('outbound message limit ignored');
 await iterator.return(undefined);if(socket.raw.readyState===1)throw new Error('breaking iteration did not close socket');
 await mustReject(()=>new Client(process.env.TREST_SOCKET_URL!,{headers:{Authorization:'Bearer fixture'}}).privateChat({path:{room:'a/b'}}),'default WebSocket silently discarded Authorization');
 const fake=new FakeSocket();let captured='';let connected=false;
 const custom=new Client('http://example.test',{headers:{Authorization:'Bearer fixture'},socketFactory:(url,{headers})=>{if(url!=='ws://example.test/private-chat/a%2Fb')throw new Error('incorrect socket URL');captured=headers.get('Authorization')??'';return fake;}});
 const opening=custom.privateChat({path:{room:'a/b'}}).then(value=>{connected=true;return value;});await Promise.resolve();if(connected)throw new Error('socket returned before opening');fake.open();const secured=await opening;
 if(captured!=='Bearer fixture')throw new Error('factory headers lost');secured.send({text:'custom'});if(fake.sent[0]!==JSON.stringify({text:'custom'}))throw new Error('typed send changed');secured.close();
 for(const test of [{name:'malformed',data:'{bad}'},{name:'size',data:'x'.repeat(65*1024)},{name:'binary',data:new Uint8Array([1])}]){
  const raw=new FakeSocket();const pending=new Client('http://example.test',{socketFactory:()=>raw}).chat();raw.open();const stream=await pending;raw.message(test.data);
  await mustReject(()=>stream.messages().next(),test.name+' message accepted');if(!raw.closed)throw new Error('invalid message retained connection');
 }
 const queueRaw=new FakeSocket();const queuePending=new Client('http://example.test',{maxQueuedMessages:1,socketFactory:()=>queueRaw}).chat();queueRaw.open();const queue=await queuePending;queueRaw.message('{"echo":"one"}');queueRaw.message('{"echo":"two"}');await mustReject(()=>queue.messages().next(),'queue limit ignored');
 const abortRaw=new FakeSocket();const controller=new AbortController();const abortPending=new Client('http://example.test',{socketFactory:()=>abortRaw}).chat({}, {signal:controller.signal});controller.abort();await mustReject(()=>abortPending,'connect abort ignored');if(!abortRaw.closed)throw new Error('aborted connect leaked');
 const activeRaw=new FakeSocket();const activeController=new AbortController();const activePending=new Client('http://example.test',{socketFactory:()=>activeRaw}).chat({}, {signal:activeController.signal});activeRaw.open();const active=await activePending;const next=active.messages().next();activeController.abort();await mustReject(()=>next,'receive abort ignored');
 const sendRaw=new FakeSocket();const sendPending=new Client('http://example.test',{maxMessageBytes:32,maxQueuedMessages:1,socketFactory:()=>sendRaw}).chat();sendRaw.open();const sending=await sendPending;sendRaw.bufferedAmount=32;limited=false;try{sending.send({text:'x'});}catch{limited=true;}if(!limited)throw new Error('send buffer limit ignored');sending.close();
}
main().catch(error=>{console.error(error);process.exitCode=1;});
`
